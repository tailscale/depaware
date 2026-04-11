package depaware

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/tools/txtar"
)

var updateGolden = flag.Bool("update-golden", false, "update golden test files")

func TestDumpSource(t *testing.T) {
	tests := []struct {
		name    string
		pkgPath string
		binName string
	}{
		{
			name:    "smallbin",
			pkgPath: "github.com/tailscale/depaware/test/smallbin",
			binName: "smallbin",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := dumpSource(tt.pkgPath)
			if err != nil {
				t.Fatal(err)
			}

			goldenFile := filepath.Join("testdata",
				tt.binName+"_"+runtime.GOOS+"_"+runtime.GOARCH+".txtar")

			if *updateGolden {
				if err := os.MkdirAll("testdata", 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(goldenFile, txtar.Format(got), 0644); err != nil {
					t.Fatal(err)
				}
				t.Logf("updated %s", goldenFile)
				return
			}

			wantData, err := os.ReadFile(goldenFile)
			if err != nil {
				t.Fatalf("reading golden file (run with -update-golden to create): %v", err)
			}
			want := txtar.Parse(wantData)

			if len(got.Files) != len(want.Files) {
				t.Fatalf("got %d files, want %d files\ngot:\n%s", len(got.Files), len(want.Files), txtar.Format(got))
			}
			for i := range got.Files {
				if got.Files[i].Name != want.Files[i].Name {
					t.Errorf("file %d: got name %q, want %q", i, got.Files[i].Name, want.Files[i].Name)
				}
				if string(got.Files[i].Data) != string(want.Files[i].Data) {
					t.Errorf("file %s content mismatch:\n--- want ---\n%s\n--- got ---\n%s",
						want.Files[i].Name, want.Files[i].Data, got.Files[i].Data)
				}
			}
		})
	}
}
