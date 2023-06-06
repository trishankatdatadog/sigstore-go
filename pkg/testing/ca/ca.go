package ca

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/digitorus/timestamp"
	"github.com/github/sigstore-verifier/pkg/root"
	"github.com/github/sigstore-verifier/pkg/tlog"
	"github.com/go-openapi/runtime"
	"github.com/secure-systems-lab/go-securesystemslib/dsse"
	cbundle "github.com/sigstore/cosign/v2/pkg/cosign/bundle"
	"github.com/sigstore/rekor/pkg/generated/models"
	"github.com/sigstore/rekor/pkg/pki"
	"github.com/sigstore/rekor/pkg/types"
	"github.com/sigstore/rekor/pkg/types/hashedrekord"
	"github.com/sigstore/rekor/pkg/types/intoto"
	"github.com/sigstore/rekor/pkg/types/rekord"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	sigdsse "github.com/sigstore/sigstore/pkg/signature/dsse"
	tsx509 "github.com/sigstore/timestamp-authority/pkg/x509"
)

type VirtualSigstore struct {
	fulcioCA              root.CertificateAuthority
	fulcioIntermediateKey *ecdsa.PrivateKey
	tsaCA                 root.CertificateAuthority
	tsaLeafKey            *ecdsa.PrivateKey
	rekorKey              *ecdsa.PrivateKey
}

func NewVirtualSigstore() (*VirtualSigstore, error) {
	ss := &VirtualSigstore{fulcioCA: root.CertificateAuthority{}, tsaCA: root.CertificateAuthority{}}

	rootCert, rootKey, err := GenerateRootCa()
	if err != nil {
		return nil, err
	}
	ss.fulcioCA.Root = rootCert
	ss.tsaCA.Root = rootCert

	intermediateCert, intermediateKey, _ := GenerateFulcioIntermediate(rootCert, rootKey)
	ss.fulcioCA.Intermediates = []*x509.Certificate{intermediateCert}
	ss.fulcioIntermediateKey = intermediateKey

	tsaIntermediateCert, tsaIntermediateKey, err := GenerateTSAIntermediate(rootCert, rootKey)
	if err != nil {
		return nil, err
	}
	ss.tsaCA.Intermediates = []*x509.Certificate{tsaIntermediateCert}
	tsaLeafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	tsaLeafCert, err := GenerateTSALeafCert(time.Now().Add(-5*time.Minute), tsaLeafKey, tsaIntermediateCert, tsaIntermediateKey)
	if err != nil {
		return nil, err
	}
	ss.tsaCA.Leaf = tsaLeafCert
	ss.tsaLeafKey = tsaLeafKey

	ss.fulcioCA.ValidityPeriodStart = time.Now().Add(-5 * time.Hour)
	ss.fulcioCA.ValidityPeriodEnd = time.Now().Add(time.Hour)
	ss.tsaCA.ValidityPeriodStart = time.Now().Add(-5 * time.Hour)
	ss.tsaCA.ValidityPeriodEnd = time.Now().Add(time.Hour)

	ss.rekorKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}

	return ss, nil
}

// getLogID calculates the digest of a PKIX-encoded public key
func getLogID(pub crypto.PublicKey) (string, error) {
	pubBytes, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(pubBytes)
	return hex.EncodeToString(digest[:]), nil
}

func (ca *VirtualSigstore) rekorSignPayload(payload cbundle.RekorPayload) ([]byte, error) {
	jsonPayload, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	canonicalized, err := jsoncanonicalizer.Transform(jsonPayload)
	if err != nil {
		return nil, err
	}
	signer, err := signature.LoadECDSASignerVerifier(ca.rekorKey, crypto.SHA256)
	if err != nil {
		return nil, err
	}
	bundleSig, err := signer.SignMessage(bytes.NewReader(canonicalized))
	if err != nil {
		return nil, err
	}
	return bundleSig, nil
}

func (ca *VirtualSigstore) GenerateLeafCert(identity, issuer string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	privKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	leafCert, err := GenerateLeafCert(identity, issuer, time.Now().Add(5*time.Minute), privKey, ca.fulcioCA.Intermediates[0], ca.fulcioIntermediateKey)
	if err != nil {
		return nil, nil, err
	}
	return leafCert, privKey, nil
}

func (ca *VirtualSigstore) Attest(identity, issuer string, envelopeBody []byte) (*TestEntity, error) {
	leafCert, leafPrivKey, err := ca.GenerateLeafCert(identity, issuer)
	if err != nil {
		return nil, err
	}

	signer, err := signature.LoadECDSASignerVerifier(leafPrivKey, crypto.SHA256)
	if err != nil {
		return nil, err
	}

	dsseSigner, err := dsse.NewEnvelopeSigner(&sigdsse.SignerAdapter{
		SignatureSigner: signer,
		Pub:             leafCert.PublicKey.(*ecdsa.PublicKey),
	})
	if err != nil {
		return nil, err
	}

	envelope, err := dsseSigner.SignPayload(context.TODO(), "application/json", envelopeBody)
	if err != nil {
		return nil, err
	}

	sig, err := base64.StdEncoding.DecodeString(envelope.Signatures[0].Sig)
	if err != nil {
		return nil, err
	}

	tsr, err := generateTimestampingResponse(sig, ca.tsaCA.Leaf, ca.tsaLeafKey)
	if err != nil {
		return nil, err
	}

	entry, err := ca.generateTlogEntry(leafCert, envelope, sig)
	if err != nil {
		return nil, err
	}

	return &TestEntity{
		certChain:   []*x509.Certificate{leafCert, ca.fulcioCA.Intermediates[0], ca.fulcioCA.Root},
		timestamps:  [][]byte{tsr},
		envelope:    envelope,
		tlogEntries: []*tlog.Entry{entry},
	}, nil
}

func (ca *VirtualSigstore) generateTlogEntry(leafCert *x509.Certificate, envelope *dsse.Envelope, sig []byte) (*tlog.Entry, error) {
	leafCertPem, err := cryptoutils.MarshalCertificateToPEM(leafCert)
	if err != nil {
		return nil, err
	}

	envelopeBytes, err := json.Marshal(envelope)
	if err != nil {
		return nil, err
	}

	rekorBody, err := generateRekorEntry(intoto.KIND, intoto.New().DefaultVersion(), envelopeBytes, leafCertPem, sig)
	if err != nil {
		return nil, err
	}

	rekorLogID, err := getLogID(ca.rekorKey.Public())
	if err != nil {
		return nil, err
	}

	rekorLogIDRaw, err := hex.DecodeString(rekorLogID)
	if err != nil {
		return nil, err
	}

	integratedTime := leafCert.NotBefore.Unix() + 1
	logIndex := int64(1000)

	b := createRekorBundle(rekorLogID, integratedTime, logIndex, rekorBody)
	set, err := ca.rekorSignPayload(*b)
	if err != nil {
		return nil, err
	}

	rekorBodyRaw, err := base64.StdEncoding.DecodeString(rekorBody)
	if err != nil {
		return nil, err
	}

	return tlog.NewEntry(rekorBodyRaw, integratedTime, logIndex, rekorLogIDRaw, set)
}

func generateRekorEntry(kind, version string, artifact []byte, cert []byte, sig []byte) (string, error) {
	// Generate the Rekor Entry
	entryImpl, err := createEntry(context.Background(), kind, version, artifact, cert, sig)
	if err != nil {
		return "", err
	}
	entryBytes, err := entryImpl.Canonicalize(context.Background())
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(entryBytes), nil
}

func createEntry(ctx context.Context, kind, apiVersion string, blobBytes, certBytes, sigBytes []byte) (types.EntryImpl, error) {
	props := types.ArtifactProperties{
		PublicKeyBytes: [][]byte{certBytes},
		PKIFormat:      string(pki.X509),
	}
	switch kind {
	case rekord.KIND, intoto.KIND:
		props.ArtifactBytes = blobBytes
		props.SignatureBytes = sigBytes
	case hashedrekord.KIND:
		blobHash := sha256.Sum256(blobBytes)
		props.ArtifactHash = strings.ToLower(hex.EncodeToString(blobHash[:]))
		props.SignatureBytes = sigBytes
	default:
		return nil, fmt.Errorf("unexpected entry kind: %s", kind)
	}
	proposedEntry, err := types.NewProposedEntry(ctx, kind, apiVersion, props)
	if err != nil {
		return nil, err
	}
	eimpl, err := types.CreateVersionedEntry(proposedEntry)
	if err != nil {
		return nil, err
	}

	can, err := types.CanonicalizeEntry(ctx, eimpl)
	if err != nil {
		return nil, err
	}
	proposedEntryCan, err := models.UnmarshalProposedEntry(bytes.NewReader(can), runtime.JSONConsumer())
	if err != nil {
		return nil, err
	}

	return types.UnmarshalEntry(proposedEntryCan)
}

func createRekorBundle(logID string, integratedTime int64, logIndex int64, rekorEntry string) *cbundle.RekorPayload {
	return &cbundle.RekorPayload{
		LogID:          logID,
		IntegratedTime: integratedTime,
		LogIndex:       logIndex,
		Body:           rekorEntry,
	}
}

func generateTimestampingResponse(sig []byte, tsaCert *x509.Certificate, tsaKey *ecdsa.PrivateKey) ([]byte, error) {
	var hash crypto.Hash
	switch tsaKey.Curve {
	case elliptic.P256():
		hash = crypto.SHA256
	case elliptic.P384():
		hash = crypto.SHA384
	case elliptic.P521():
		hash = crypto.SHA512
	}
	tsq, err := timestamp.CreateRequest(bytes.NewReader(sig), &timestamp.RequestOptions{
		Hash: hash,
	})
	if err != nil {
		return nil, err
	}

	req, err := timestamp.ParseRequest([]byte(tsq))
	if err != nil {
		return nil, err
	}

	tsTemplate := timestamp.Timestamp{
		HashAlgorithm:   req.HashAlgorithm,
		HashedMessage:   req.HashedMessage,
		Time:            time.Now(),
		Policy:          asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 2},
		Ordering:        false,
		Qualified:       false,
		ExtraExtensions: req.Extensions,
	}

	return tsTemplate.CreateResponse(tsaCert, tsaKey)
}

func (ca *VirtualSigstore) TSACertificateAuthorities() []root.CertificateAuthority {
	return []root.CertificateAuthority{ca.tsaCA}
}

func (ca *VirtualSigstore) FulcioCertificateAuthorities() []root.CertificateAuthority {
	return []root.CertificateAuthority{ca.fulcioCA}
}

func (ca *VirtualSigstore) TlogVerifiers() map[string]*root.TlogVerifier {
	verifiers := make(map[string]*root.TlogVerifier)
	logID, err := getLogID(ca.rekorKey.Public())
	if err != nil {
		panic(err)
	}
	verifiers[logID] = &root.TlogVerifier{
		BaseURL:             "test",
		ID:                  []byte(logID),
		ValidityPeriodStart: time.Now().Add(-time.Hour),
		ValidityPeriodEnd:   time.Now().Add(time.Hour),
		HashFunc:            crypto.SHA256,
		PublicKey:           ca.rekorKey.Public(),
	}
	return verifiers
}

type TestEntity struct {
	certChain   []*x509.Certificate
	envelope    *dsse.Envelope
	timestamps  [][]byte
	tlogEntries []*tlog.Entry
	keyID       string
}

func (e *TestEntity) CertificateChain() ([]*x509.Certificate, error) {
	return e.certChain, nil
}

func (e *TestEntity) Envelope() (*dsse.Envelope, error) {
	return e.envelope, nil
}

func (e *TestEntity) Timestamps() ([][]byte, error) {
	return e.timestamps, nil
}

func (e *TestEntity) TlogEntries() ([]*tlog.Entry, error) {
	return e.tlogEntries, nil
}

func (e *TestEntity) KeyID() (string, error) {
	return e.keyID, nil
}

// Much of the following code is adapted from cosign/test/cert_utils.go

func createCertificate(template *x509.Certificate, parent *x509.Certificate, pub interface{}, priv crypto.Signer) (*x509.Certificate, error) {
	certBytes, err := x509.CreateCertificate(rand.Reader, template, parent, pub, priv)
	if err != nil {
		return nil, err
	}

	cert, err := x509.ParseCertificate(certBytes)
	if err != nil {
		return nil, err
	}
	return cert, nil
}

func GenerateRootCa() (*x509.Certificate, *ecdsa.PrivateKey, error) {
	rootTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "sigstore",
			Organization: []string{"sigstore.dev"},
		},
		NotBefore:             time.Now().Add(-5 * time.Hour),
		NotAfter:              time.Now().Add(5 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	cert, err := createCertificate(rootTemplate, rootTemplate, &priv.PublicKey, priv)
	if err != nil {
		return nil, nil, err
	}

	return cert, priv, nil
}

func GenerateFulcioIntermediate(rootTemplate *x509.Certificate, rootPriv crypto.Signer) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	subTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "sigstore-intermediate",
			Organization: []string{"sigstore.dev"},
		},
		NotBefore:             time.Now().Add(-2 * time.Minute),
		NotAfter:              time.Now().Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	cert, err := createCertificate(subTemplate, rootTemplate, &priv.PublicKey, rootPriv)
	if err != nil {
		return nil, nil, err
	}

	return cert, priv, nil
}

func GenerateTSAIntermediate(rootTemplate *x509.Certificate, rootPriv crypto.Signer) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	subTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject: pkix.Name{
			CommonName:   "sigstore-tsa-intermediate",
			Organization: []string{"sigstore.dev"},
		},
		NotBefore:             time.Now().Add(-2 * time.Minute),
		NotAfter:              time.Now().Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}

	cert, err := createCertificate(subTemplate, rootTemplate, &priv.PublicKey, rootPriv)
	if err != nil {
		return nil, nil, err
	}

	return cert, priv, nil
}

func GenerateLeafCert(subject string, oidcIssuer string, expiration time.Time, priv *ecdsa.PrivateKey,
	parentTemplate *x509.Certificate, parentPriv crypto.Signer) (*x509.Certificate, error) {
	certTemplate := &x509.Certificate{
		SerialNumber:   big.NewInt(1),
		EmailAddresses: []string{subject},
		NotBefore:      expiration,
		NotAfter:       expiration.Add(10 * time.Minute),
		KeyUsage:       x509.KeyUsageDigitalSignature,
		ExtKeyUsage:    []x509.ExtKeyUsage{x509.ExtKeyUsageCodeSigning},
		IsCA:           false,
		ExtraExtensions: []pkix.Extension{{
			// OID for OIDC Issuer extension
			Id:       asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 57264, 1, 1},
			Critical: false,
			Value:    []byte(oidcIssuer),
		},
		},
	}

	cert, err := createCertificate(certTemplate, parentTemplate, &priv.PublicKey, parentPriv)
	if err != nil {
		return nil, err
	}

	return cert, nil
}

func GenerateTSALeafCert(expiration time.Time, priv *ecdsa.PrivateKey, parentTemplate *x509.Certificate, parentPriv crypto.Signer) (*x509.Certificate, error) {
	timestampExt, err := asn1.Marshal([]asn1.ObjectIdentifier{tsx509.EKUTimestampingOID})
	if err != nil {
		return nil, err
	}

	certTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    expiration,
		NotAfter:     expiration.Add(10 * time.Minute),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
		IsCA:         false,
		// set EKU to x509.ExtKeyUsageTimeStamping but with a critical bit
		ExtraExtensions: []pkix.Extension{
			{
				Id:       asn1.ObjectIdentifier{2, 5, 29, 37},
				Critical: true,
				Value:    timestampExt,
			},
		},
	}

	cert, err := createCertificate(certTemplate, parentTemplate, &priv.PublicKey, parentPriv)
	if err != nil {
		return nil, err
	}

	return cert, nil
}