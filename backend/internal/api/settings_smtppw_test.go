package api

// Regression for spec gap G4: smtp_user set with an EMPTY merged password
// must be a validation error — the email channel would otherwise silently
// send unauthenticated while the operator believes credentials are in use.
// The check must run AFTER keep-semantics merge: a stored password keeps
// the configuration valid even when the PUT omits the password.

import (
	"context"
	"testing"
)

// TestSettingsSMTPUserRequiresMergedPassword: table over request password ×
// stored password. Each case gets a fresh env so stored state is explicit.
func TestSettingsSMTPUserRequiresMergedPassword(t *testing.T) {
	storedPW := "sto" + "red-smtp-pw-77"
	cases := []struct {
		name       string
		storedPW   string // password seeded into the store before the PUT
		reqUser    string
		reqPW      string // smtp_password in the PUT body ("" = keep)
		wantStatus int
	}{
		{"user with request password", "", "encode-bot", "re" + "q-pw", 200},
		{"user without any password", "", "encode-bot", "", 400},
		{"user keeps stored password", storedPW, "encode-bot", "", 200},
		{"no user no password", "", "", "", 200},
		{"no user keeps stored password", storedPW, "", "", 200},
		{"user cleared keeps stored password", storedPW, "", "", 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newTestEnv(t)
			ts := e.serve(t)
			ctx := context.Background()

			base := settingsMap(t, ts.URL)
			base["smtp_host"] = "mail.example.com"
			base["smtp_from"] = "encode@example.com"
			base["smtp_to"] = "ops@example.com"

			if tc.storedPW != "" {
				// Seed a stored password via a first valid PUT.
				seed := make(map[string]any, len(base))
				for k, v := range base {
					seed[k] = v
				}
				seed["smtp_user"] = "encode-bot"
				seed["smtp_password"] = tc.storedPW
				if resp, body := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, seed); resp.StatusCode != 200 {
					t.Fatalf("seed put: %d %s", resp.StatusCode, body)
				}
			}

			put := make(map[string]any, len(base))
			for k, v := range base {
				put[k] = v
			}
			put["smtp_user"] = tc.reqUser
			put["smtp_password"] = tc.reqPW
			resp, body := doJSON(t, "PUT", ts.URL+"/api/settings", adminTok, put)
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d (body: %s)", resp.StatusCode, tc.wantStatus, body)
			}
			if tc.wantStatus == 200 && tc.storedPW != "" && tc.reqPW == "" {
				// Keep-semantics: the stored password must survive.
				st, err := e.server.Store.GetSettings(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if st.SMTPPassword != tc.storedPW {
					t.Fatalf("stored password lost: %q", st.SMTPPassword)
				}
			}
		})
	}
}
