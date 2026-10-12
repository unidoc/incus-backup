package api

import "time"

// RenewRequest is POST /v1/renew, authenticated by the certificate being renewed.
// The subject of the CSR is ignored (the server keeps the identity's name).
type RenewRequest struct {
	CSR string `json:"csr"` // PEM
}

// RenewResponse carries the new certificate. CA is the server's trust bundle
// (the CA it presents first, then any CA introduced by a rotation); clients replace
// their stored CA file with it.
type RenewResponse struct {
	Certificate string    `json:"certificate"` // PEM
	CA          string    `json:"ca"`          // PEM bundle
	NotAfter    time.Time `json:"not_after"`
	Host        string    `json:"host"`
	Scope       string    `json:"scope"`
	Repository  string    `json:"repository"`
	// SigningCA is the fingerprint of the CA that signs client certificates now.
	SigningCA string `json:"signing_ca"`
}

// RenewConfirm is POST /v1/renew/confirm, authenticated by the NEW certificate:
// every other certificate of the identity is revoked.
type RenewConfirm struct{}

// RenewConfirmResponse reports how many older certificates were revoked.
type RenewConfirmResponse struct {
	Revoked int `json:"revoked"`
}

// CertInfo is GET /v1/cert: what the server knows about the presented certificate.
type CertInfo struct {
	Fingerprint string    `json:"fingerprint"`
	NotAfter    time.Time `json:"not_after"`
	Current     bool      `json:"current"`
	// SigningCA: the CA new client certificates are signed by. A client whose
	// certificate was signed by another CA renews early (a CA rotation is under way).
	SigningCA string    `json:"signing_ca"`
	ServerNow time.Time `json:"server_now"`
}
