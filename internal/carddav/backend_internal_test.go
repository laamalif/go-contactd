package carddav

import (
	"context"
	"reflect"
	"testing"
)

func TestParseCardPathForPrincipal_RejectsTraversalAndSeparators(t *testing.T) {
	t.Parallel()

	ctx := WithPrincipal(context.Background(), "alice")

	tests := []struct {
		name   string
		path   string
		status int
	}{
		{name: "cross_tenant", path: "/mallory/contacts/a.vcf", status: 404},
		{name: "dot_href", path: "/alice/contacts/.", status: 404},     // path.Clean collapses to collection path
		{name: "dotdot_href", path: "/alice/contacts/..", status: 404}, // path.Clean collapses path segments
		{name: "backslash_href", path: "/alice/contacts/a\\b.vcf", status: 400},
		{name: "nested_href", path: "/alice/contacts/a/b.vcf", status: 404}, // extra segment count mismatch
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, _, _, err := parseCardPathForPrincipal(ctx, tt.path)
			if err == nil {
				t.Fatalf("parseCardPathForPrincipal(%q) error=nil, want HTTP %d", tt.path, tt.status)
			}
			if got, want := statusFromErr(err), tt.status; got != want {
				t.Fatalf("status = %d, want %d; err=%v", got, want, err)
			}
		})
	}
}

func TestParseCardPathForPrincipal_AllowsSafeFlatHref(t *testing.T) {
	t.Parallel()

	ctx := WithPrincipal(context.Background(), "alice")
	user, slug, href, err := parseCardPathForPrincipal(ctx, "/alice/contacts/a..b.vcf")
	if err != nil {
		t.Fatalf("parseCardPathForPrincipal safe href: %v", err)
	}
	if user != "alice" || slug != "contacts" || href != "a..b.vcf" {
		t.Fatalf("parsed = (%q,%q,%q), want (alice,contacts,a..b.vcf)", user, slug, href)
	}
}

func statusFromErr(err error) int {
	if err == nil {
		return 0
	}
	if he, ok := err.(interface{ StatusCode() int }); ok {
		return he.StatusCode()
	}
	for err != nil {
		v := reflect.ValueOf(err)
		if v.IsValid() && v.Kind() == reflect.Ptr && !v.IsNil() {
			elem := v.Elem()
			if elem.IsValid() && elem.Kind() == reflect.Struct {
				code := elem.FieldByName("Code")
				if code.IsValid() && code.CanInt() {
					return int(code.Int())
				}
			}
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return 0
		}
		err = u.Unwrap()
	}
	return 0
}
