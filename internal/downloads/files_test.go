package downloads

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"animind-backend-go/internal/config"
)

func TestSelectableFilesRules(t *testing.T) {
	cfg := config.DownloadsConfig{MaxFiles: 500, Dir: t.TempDir()}
	files := []EngineFile{
		{Index: 0, Path: "Show - 01 [1080p].mkv", SizeBytes: 100},
		{Index: 1, Path: "Show - 01.srt", SizeBytes: 2},
		{Index: 2, Path: "samples/sample.jpg", SizeBytes: 2},
		{Index: 3, Path: "../outside.mkv", SizeBytes: 2},
		{Index: 4, Path: "Show - 02.mkv", SizeBytes: 100},
		{Index: 5, Path: "Show - 03.nfo", SizeBytes: 2},
	}
	got, err := SelectableFiles(files, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int{0, 1, 4}) {
		t.Fatalf("selection: %v", got)
	}
	// hostile names are errors, not skips: they must never touch disk
	if _, err := SelectableFiles([]EngineFile{{Index: 9, Path: "CON.mkv"}}, cfg); err == nil {
		t.Fatal("windows device name must be rejected")
	}
	if _, err := SelectableFiles([]EngineFile{{Index: 9, Path: "subs/COM1.ass"}}, cfg); err == nil {
		t.Fatal("device name in a subdirectory must be rejected too")
	}
	if _, err := SelectableFiles([]EngineFile{{Index: 9, Path: strings.Repeat("a", 300) + ".mkv"}}, cfg); err == nil {
		t.Fatal("overlong path must be rejected")
	}
}

func TestSelectableFilesCapsCount(t *testing.T) {
	cfg := config.DownloadsConfig{MaxFiles: 3, Dir: t.TempDir()}
	var files []EngineFile
	for i := 0; i < 10; i++ {
		files = append(files, EngineFile{Index: i, Path: fmt.Sprintf("ep%02d.mkv", i)})
	}
	if got, _ := SelectableFiles(files, cfg); len(got) != 3 {
		t.Fatalf("cap: %v", got)
	}
}

func TestSelectableFilesKeepsLargest(t *testing.T) {
	cfg := config.DownloadsConfig{MaxFiles: 2, Dir: t.TempDir()}
	files := []EngineFile{
		{Index: 0, Path: "a.mkv", SizeBytes: 10},
		{Index: 1, Path: "b.mkv", SizeBytes: 300},
		{Index: 2, Path: "c.mkv", SizeBytes: 200},
	}
	got, err := SelectableFiles(files, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []int{1, 2}) {
		t.Fatalf("largest files must win the cap: %v", got)
	}
}
