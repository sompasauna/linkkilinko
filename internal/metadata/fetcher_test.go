package metadata_test

import (
	"net"
	"testing"

	"github.com/sompasauna/linkkilinko/internal/metadata"
)

func TestIsPublicIPRejectsSpecialPurposeRanges(t *testing.T) {
	tests := []string{
		"0.1.2.3", "10.0.0.1", "100.64.0.1", "192.0.2.1", "198.51.100.1",
		"203.0.113.1", "240.0.0.1", "2001:db8::1", "fc00::1", "fe80::1",
	}
	for _, value := range tests {
		if metadata.IsPublicIP(net.ParseIP(value)) {
			t.Errorf("isPublicIP(%q) = true", value)
		}
	}
	for _, value := range []string{"1.1.1.1", "2001:4860:4860::8888"} {
		if !metadata.IsPublicIP(net.ParseIP(value)) {
			t.Errorf("isPublicIP(%q) = false", value)
		}
	}
}
