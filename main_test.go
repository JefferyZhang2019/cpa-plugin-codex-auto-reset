package main

import "testing"

func TestPluginRegistration(t *testing.T) {
	reg := pluginRegistration()
	if reg.Metadata.Name != "Codex Auto Reset" {
		t.Fatalf("name = %q", reg.Metadata.Name)
	}
	if reg.Capabilities.ManagementAPI != true {
		t.Fatalf("management API not enabled")
	}
	if reg.SchemaVersion == 0 {
		t.Fatalf("schema version unset")
	}
}

func TestManagementRegistrationResponse_HasExpectedRoutes(t *testing.T) {
	resp := managementRegistrationResponse()
	if len(resp.Routes) < 8 {
		t.Fatalf("expected >=8 routes, got %d", len(resp.Routes))
	}
	if len(resp.Resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resp.Resources))
	}
	if resp.Resources[0].Path != "/status" {
		t.Fatalf("resource path = %q", resp.Resources[0].Path)
	}
}

func TestHandleMethod_UnknownReturnsError(t *testing.T) {
	// configurePlugin requires host callbacks; skip wiring and just check unknown method.
	raw, err := handleMethod("totally.unknown", nil)
	if err != nil {
		t.Fatalf("unknown method should not return Go error: %v", err)
	}
	if len(raw) == 0 {
		t.Fatalf("expected envelope body")
	}
	// Body should contain the error code.
	body := string(raw)
	if !contains(body, "unknown_method") {
		t.Fatalf("body missing error code: %s", body)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
