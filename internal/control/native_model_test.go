package control

import "testing"

func TestNativeModelCatalogBinding(t *testing.T) {
	c := validCatalog()
	c.Profiles[0].NativeModel = &NativeModel{Runtime: "ollama", Instance: "local", Model: "a:latest", Endpoint: "http://127.0.0.1:11434", LaunchFile: "/units/a.service", LaunchSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	clone := c.Clone()
	clone.Profiles[0].NativeModel.Model = "b"
	if c.Profiles[0].NativeModel.Model != "a:latest" {
		t.Fatal("clone aliases model")
	}
	c.Profiles[0].NativeModel.Runtime = "arbitrary"
	if c.Validate() == nil {
		t.Fatal("unsupported native runtime accepted")
	}
}
func TestNativeModelRequest(t *testing.T) {
	for _, body := range []string{`{"model":"selected","Model":"other"}`, `{"model":"a","Keep_Alive":0}`, `{"model":"other"}`, `{"model":"a","model":"other"}`, `{"model":"a","keep_alive":0}`, `{}`, `{"model":"a"} {}`} {
		if ValidateModelRequest([]byte(body), "a") == nil {
			t.Fatalf("accepted %s", body)
		}
	}
	if err := ValidateModelRequest([]byte(`{"model":"a","messages":[]}`), "a"); err != nil {
		t.Fatal(err)
	}
}
