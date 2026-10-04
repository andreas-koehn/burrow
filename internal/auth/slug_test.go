package auth

import "testing"

func TestValidSlug(t *testing.T) {
	ok := []string{"abc", "p7baeh", "ollama", "my-app-2", "a1b", "a23456789012345678901234567890123456789b"}
	bad := []string{"", "ab", "-abc", "abc-", "Abc", "a_b", "a.b", "a/b", "a b", "ä-b", "a23456789012345678901234567890123456789bc"}
	for _, s := range ok {
		if !ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = false, want true", s)
		}
	}
	for _, s := range bad {
		if ValidSlug(s) {
			t.Errorf("ValidSlug(%q) = true, want false", s)
		}
	}
}

func TestGenerateSlug_IsValidAndVaries(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		s, err := GenerateSlug()
		if err != nil {
			t.Fatal(err)
		}
		if len(s) != 6 || !ValidSlug(s) {
			t.Fatalf("GenerateSlug() = %q, want 6 valid chars", s)
		}
		seen[s] = true
	}
	if len(seen) < 45 {
		t.Fatalf("only %d distinct slugs in 50 draws", len(seen))
	}
}
