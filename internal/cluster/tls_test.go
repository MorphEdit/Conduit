package cluster

import (
	"testing"

	"github.com/conduit-sync/conduit/internal/peer"
)

func TestGeneratedCertificateFingerprint(t *testing.T) {
	m, err := GenerateTLS("a")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := m.ServerConfig()
	if err != nil {
		t.Fatal(err)
	}
	if got := peer.Fingerprint(cfg.Certificates[0].Certificate[0]); got != m.Fingerprint {
		t.Fatalf("fingerprint mismatch %s != %s", got, m.Fingerprint)
	}
}

func TestInviteRoundTrip(t *testing.T) {
	in := Invite{URL: "https://host:7443", Secret: "abc", Fingerprint: "ff00"}
	out, err := DecodeInvite(in.Encode())
	if err != nil || out != in {
		t.Fatalf("got %+v, %v", out, err)
	}
	if _, err := DecodeInvite("cdt1_!!!"); err == nil {
		t.Fatal("damaged code must fail")
	}
}
