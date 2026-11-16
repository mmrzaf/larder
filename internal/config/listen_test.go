package config

import "testing"

func TestValidateListen(t *testing.T) {
	tests := []struct {
		value    string
		optional bool
		wantErr  bool
	}{
		{"0.0.0.0:8080", false, false},
		{"127.0.0.1:0", false, false},
		{"[::1]:9090", false, false},
		{":8080", false, false},
		{"", true, false},
		{"", false, true},
		{"not-an-address", false, true},
		{"localhost", false, true},
		{"127.0.0.1:http-alt", false, true},
		{"127.0.0.1:70000", false, true},
	}
	for _, test := range tests {
		err := validateListen(test.value, test.optional)
		if (err != nil) != test.wantErr {
			t.Errorf("validateListen(%q, %v) = %v, wantErr %v", test.value, test.optional, err, test.wantErr)
		}
	}
}
