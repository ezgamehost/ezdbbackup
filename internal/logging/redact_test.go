package logging

import "testing"

type customCredential string

func (customCredential) MarshalJSON() ([]byte, error) {
	return []byte(`{"API_TOKEN":"hidden","safe":"visible"}`), nil
}

func TestRedactRecursivelyMatchesSecretKeysCaseInsensitively(t *testing.T) {
	original := map[string]any{
		"Password": "one",
		"nested": map[string]any{
			"api_secret_value": "two",
			"authTOKEN":        "three",
			"CredentialData":   "four",
			"MYSQL_PWD":        "five",
			"safe":             "visible",
		},
		"items": []any{map[string]any{"refresh_token": "six"}},
	}

	got := redactFields(original)
	if got["Password"] != "[REDACTED]" {
		t.Fatalf("Password = %v", got["Password"])
	}
	nested := got["nested"].(map[string]any)
	for _, key := range []string{"api_secret_value", "authTOKEN", "CredentialData", "MYSQL_PWD"} {
		if nested[key] != "[REDACTED]" {
			t.Errorf("%s = %v", key, nested[key])
		}
	}
	if nested["safe"] != "visible" {
		t.Errorf("safe = %v", nested["safe"])
	}
	item := got["items"].([]any)[0].(map[string]any)
	if item["refresh_token"] != "[REDACTED]" {
		t.Errorf("refresh_token = %v", item["refresh_token"])
	}
}

func TestRedactReturnsDeepCopyWithoutMutatingInput(t *testing.T) {
	nested := map[string]any{"safe": "before", "password": "original"}
	original := map[string]any{"nested": nested}

	got := redactFields(original)
	gotNested := got["nested"].(map[string]any)
	gotNested["safe"] = "after"

	if nested["safe"] != "before" || nested["password"] != "original" {
		t.Fatalf("input mutated: %#v", original)
	}
}

func TestRedactTraversesTypedNestedCollections(t *testing.T) {
	typed := map[string]string{"AccessToken": "hidden", "safe": "visible"}
	original := map[string]any{"typed": typed, "items": []map[string]string{{"db_password": "hidden"}}}

	got := redactFields(original)
	gotTyped := got["typed"].(map[string]any)
	if gotTyped["AccessToken"] != "[REDACTED]" || gotTyped["safe"] != "visible" {
		t.Fatalf("typed map = %#v", gotTyped)
	}
	gotItem := got["items"].([]any)[0].(map[string]any)
	if gotItem["db_password"] != "[REDACTED]" {
		t.Fatalf("typed slice item = %#v", gotItem)
	}
	if typed["AccessToken"] != "hidden" {
		t.Fatalf("input mutated: %#v", typed)
	}
}

func TestRedactDoesNotBypassSecretsInDeeplyNestedValues(t *testing.T) {
	original := make(map[string]any)
	cursor := original
	for range 70 {
		next := make(map[string]any)
		cursor["next"] = next
		cursor = next
	}
	cursor["Password"] = "hidden"

	got := redactFields(original)
	gotCursor := got
	for range 70 {
		gotCursor = gotCursor["next"].(map[string]any)
	}
	if gotCursor["Password"] != "[REDACTED]" {
		t.Fatalf("deep Password = %v", gotCursor["Password"])
	}
}

func TestRedactTraversesCustomJSONValues(t *testing.T) {
	got := redactFields(map[string]any{"custom": customCredential("opaque")})
	custom := got["custom"].(map[string]any)
	if custom["API_TOKEN"] != "[REDACTED]" || custom["safe"] != "visible" {
		t.Fatalf("custom field = %#v", custom)
	}
}
