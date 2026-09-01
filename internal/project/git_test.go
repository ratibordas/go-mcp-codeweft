package project

import "testing"

func TestParsePorcelainV2Rename(t *testing.T) {
	in := []byte("2 R. N... 100644 100644 100644 abc def R100 new name.ts\x00old name.ts\x00")
	got, err := parsePorcelainV2(in)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Renames) != 1 || got.Renames[0].Old != "old name.ts" || got.Renames[0].New != "new name.ts" {
		t.Fatalf("unexpected rename: %+v", got.Renames)
	}
}

func TestParsePorcelainV2PathsHandlesSpacesUnicodeAndMultipleRecords(t *testing.T) {
	in := []byte("1 .M N... 100644 100644 100644 abc def name with spaces.go\x00? café.ts\x00u UU N... 100644 100644 100644 100644 abc def 0 moved.go\x00")
	got, err := parsePorcelainV2(in)
	if err != nil {
		t.Fatal(err)
	}
	if paths := got.Paths; len(paths) != 3 || paths[0] != "café.ts" || paths[1] != "moved.go" || paths[2] != "name with spaces.go" {
		t.Fatalf("unexpected paths: %q", paths)
	}
}
