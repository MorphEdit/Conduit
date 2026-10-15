// Copyright 2026 MorphEdit (https://github.com/MorphEdit). All rights reserved.
// Licensed under the PolyForm Strict License 1.0.0 - see LICENSE and NOTICE.
// Required Notice: Copyright 2026 MorphEdit (https://github.com/MorphEdit)

package server

import "testing"

func TestKnownHost(t *testing.T) {
	names := dashboardHosts("https://office.example.com:7443", []string{"Conduit.Corp.Lan"})
	for _, h := range []string{
		"127.0.0.1:7420", "192.168.1.20:7420", "[::1]:7420", "localhost:7420", "app.localhost",
		"office.example.com:7420", "office:7420", "conduit.corp.lan", "CONDUIT.corp.lan.:80",
	} {
		if !knownHost(h, names) {
			t.Errorf("%q should be accepted", h)
		}
	}
	// A rebinding page uses its own domain name.
	for _, h := range []string{"evil.example:7420", "office.example.com.evil.example", "", "localhost.evil.example"} {
		if knownHost(h, names) {
			t.Errorf("%q must be refused", h)
		}
	}
}
