package resource

import "testing"

func TestModulePath(t *testing.T) {
	for _, valid := range []string{"", "modules/aws-s3", "test/modules/aws-s3", "a.b_c-d"} {
		if err := ModulePath(valid); err != nil {
			t.Errorf("rejected %q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"/abs", "../up", "a/../b", "a/./b", ".", "..", "a/", "a//b", ".git/hooks", "a b", `a\b`, "modules/$(x)"} {
		if ModulePath(invalid) == nil {
			t.Errorf("accepted %q", invalid)
		}
	}
}
