package depaware

import (
	"flag"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestDumpSourceGOOSDCE(t *testing.T) {
	ar, err := dumpSource("github.com/tailscale/depaware/test/platbin")
	if err != nil {
		t.Fatal(err)
	}

	// Find the platbin source file.
	var src string
	for _, f := range ar.Files {
		if strings.HasSuffix(f.Name, "platbin/platbin.go") {
			src = string(f.Data)
			break
		}
	}
	if src == "" {
		t.Fatal("platbin/platbin.go not found in output")
	}

	// The android branches should be stripped (we're building for linux).
	if strings.Contains(src, `"android"`) {
		t.Errorf("android branch not stripped:\n%s", src)
	}
	// The linux branches should be kept.
	if !strings.Contains(src, `"not android"`) {
		t.Errorf("expected 'not android' to be kept:\n%s", src)
	}
	if !strings.Contains(src, `"linux"`) {
		t.Errorf("expected 'linux' to be kept:\n%s", src)
	}
	// runtime.GOOS comparisons should be gone.
	if strings.Contains(src, "runtime.GOOS") {
		t.Errorf("runtime.GOOS comparison not stripped:\n%s", src)
	}
	// User-defined const debug=false: the "debug mode" branch should be stripped.
	if strings.Contains(src, `"debug mode"`) {
		t.Errorf("debug branch not stripped:\n%s", src)
	}
	if !strings.Contains(src, `"release mode"`) {
		t.Errorf("expected 'release mode' to be kept:\n%s", src)
	}
	// "debug && v.check()" where debug=false: whole if is false.
	if strings.Contains(src, `"debug and check"`) {
		t.Errorf("debug&&check branch not stripped:\n%s", src)
	}
	if !strings.Contains(src, `"no debug"`) {
		t.Errorf("expected 'no debug' to be kept:\n%s", src)
	}
	// "!debug && runtime.GOOS == 'linux'" where debug=false:
	// !false=true, and GOOS=="linux" is true → whole thing is true.
	if !strings.Contains(src, `"not debug and linux"`) {
		t.Errorf("expected 'not debug and linux' to be kept:\n%s", src)
	}
	// "!debug || ..." where debug=false: !false=true → whole thing is true.
	if !strings.Contains(src, `"not debug or android"`) {
		t.Errorf("expected 'not debug or android' to be kept:\n%s", src)
	}
	if strings.Contains(src, `"neither"`) {
		t.Errorf("'neither' branch should be stripped:\n%s", src)
	}
}
