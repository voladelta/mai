package mai

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestAccountIDFromJWT(t *testing.T) {
	token := func(payload string) string {
		return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	}
	for name, test := range map[string]struct {
		token   string
		want    string
		wantErr string
	}{
		"valid": {
			token:   token(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acct"}}`),
			want:    "acct",
			wantErr: "",
		},
		"not three parts": {
			token:   "header.payload",
			wantErr: "not a JWT",
		},
		"bad base64 payload": {
			token:   "header.%%%.signature",
			wantErr: "decode JWT",
		},
		"payload not JSON": {
			token:   token("not json"),
			wantErr: "parse JWT",
		},
		"missing auth claim": {
			token:   token(`{"sub":"user"}`),
			wantErr: "no chatgpt_account_id",
		},
		"account id not a string": {
			token:   token(`{"https://api.openai.com/auth":{"chatgpt_account_id":42}}`),
			wantErr: "no chatgpt_account_id",
		},
		"account id empty": {
			token:   token(`{"https://api.openai.com/auth":{"chatgpt_account_id":""}}`),
			wantErr: "no chatgpt_account_id",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := accountIDFromJWT(test.token)
			if test.wantErr == "" {
				if err != nil || got != test.want {
					t.Fatalf("accountIDFromJWT = %q, %v; want %q", got, err, test.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("accountIDFromJWT error = %v, want %q", err, test.wantErr)
			}
		})
	}
}
