package customer

import "testing"

func TestCreate(t *testing.T) {
	if got := (Service{}).Create("Ada"); got.Name != "Ada" {
		t.Fatalf("name = %q", got.Name)
	}
}
