package runko

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The conventions ledger is only worth having if it cannot drift from the
// code and docs that cite it. These tests fail the build when it does.

var convRefPattern = regexp.MustCompile(`CONV-\d+`)

// convHeadingPattern matches a convention's defining heading in
// CONVENTIONS.md, e.g. "## CONV-04 — Security headers on every response".
var convHeadingPattern = regexp.MustCompile(`(?m)^## (CONV-\d+)\b`)

// filesCitingConventions returns every file that may reference a CONV id.
func filesCitingConventions(t *testing.T) []string {
	t.Helper()

	var files []string
	roots := []string{".", "example", "scaffold", filepath.Join("scaffold", "static")}

	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue // optional directory
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			switch strings.ToLower(filepath.Ext(e.Name())) {
			case ".go", ".md", ".html":
				files = append(files, filepath.Join(root, e.Name()))
			}
		}
	}

	if len(files) == 0 {
		t.Fatal("found no files to scan for convention references")
	}
	return files
}

func definedConventions(t *testing.T) map[string]bool {
	t.Helper()

	data, err := os.ReadFile("CONVENTIONS.md")
	if err != nil {
		t.Fatalf("CONVENTIONS.md is the source of truth for conventions and must exist: %v", err)
	}

	defined := make(map[string]bool)
	for _, m := range convHeadingPattern.FindAllStringSubmatch(string(data), -1) {
		if defined[m[1]] {
			t.Errorf("%s is defined twice in CONVENTIONS.md", m[1])
		}
		defined[m[1]] = true
	}

	if len(defined) == 0 {
		t.Fatal("CONVENTIONS.md defines no conventions — expected '## CONV-nn' headings")
	}
	return defined
}

// Every CONV id cited anywhere in the repo must be defined in
// CONVENTIONS.md. Without this, a doc comment or demo card can promise a
// convention that no longer exists — the exact drift the ledger prevents.
func TestConventions_EveryReferenceIsDefined(t *testing.T) {
	defined := definedConventions(t)

	type ref struct{ id, file string }
	var unknown []ref

	for _, path := range filesCitingConventions(t) {
		if filepath.Base(path) == "CONVENTIONS.md" {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, id := range convRefPattern.FindAllString(string(data), -1) {
			if !defined[id] {
				unknown = append(unknown, ref{id, path})
			}
		}
	}

	sort.Slice(unknown, func(i, j int) bool { return unknown[i].id < unknown[j].id })
	for _, u := range unknown {
		t.Errorf("%s references %s, which is not defined in CONVENTIONS.md", u.file, u.id)
	}
}

// A convention nobody cites is a promise with no home in the code or the
// docs — either it should be enforced somewhere, demonstrated somewhere, or
// removed. Deferred conventions are exempt: having no implementation is
// precisely what they record.
func TestConventions_EveryDefinitionIsCited(t *testing.T) {
	defined := definedConventions(t)

	data, err := os.ReadFile("CONVENTIONS.md")
	if err != nil {
		t.Fatalf("read CONVENTIONS.md: %v", err)
	}
	deferred := deferredConventions(string(data))

	cited := make(map[string]bool)
	for _, path := range filesCitingConventions(t) {
		if filepath.Base(path) == "CONVENTIONS.md" {
			continue
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, id := range convRefPattern.FindAllString(string(body), -1) {
			cited[id] = true
		}
	}

	var orphans []string
	for id := range defined {
		if !cited[id] && !deferred[id] {
			orphans = append(orphans, id)
		}
	}
	sort.Strings(orphans)

	for _, id := range orphans {
		t.Errorf("%s is defined in CONVENTIONS.md but cited nowhere — enforce it, demonstrate it, mark it Deferred, or remove it", id)
	}
}

// A "Defended by" citation is an assurance that a named test protects the
// convention. If that test is renamed or deleted, the assurance silently
// becomes a lie — the most damaging failure mode this ledger has, because
// the claim still reads as verified. Check every cited name exists.
func TestConventions_CitedTestsExist(t *testing.T) {
	data, err := os.ReadFile("CONVENTIONS.md")
	if err != nil {
		t.Fatalf("read CONVENTIONS.md: %v", err)
	}

	existing := make(map[string]bool)
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	funcPattern := regexp.MustCompile(`(?m)^func (Test[A-Za-z0-9_]+)\(`)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		body, err := os.ReadFile(e.Name())
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, m := range funcPattern.FindAllStringSubmatch(string(body), -1) {
			existing[m[1]] = true
		}
	}
	if len(existing) == 0 {
		t.Fatal("found no test functions to match citations against")
	}

	// "**Defended by:** `TestOne`, `TestTwo`" — possibly wrapped over lines.
	defendedPattern := regexp.MustCompile(`(?s)\*\*Defended by:\*\*(.*?)\n\n`)
	testRefPattern := regexp.MustCompile("`(Test[A-Za-z0-9_]+)`")

	for _, block := range defendedPattern.FindAllStringSubmatch(string(data), -1) {
		names := testRefPattern.FindAllStringSubmatch(block[1], -1)
		if len(names) == 0 {
			t.Errorf("a 'Defended by' line cites no test: %q", strings.TrimSpace(block[1]))
			continue
		}
		for _, n := range names {
			if !existing[n[1]] {
				t.Errorf("CONVENTIONS.md cites %s as a defending test, but no such test exists", n[1])
			}
		}
	}
}

// "Enforced by" citations must point at files that exist, so a rename
// cannot leave the ledger pointing into space.
func TestConventions_CitedFilesExist(t *testing.T) {
	data, err := os.ReadFile("CONVENTIONS.md")
	if err != nil {
		t.Fatalf("read CONVENTIONS.md: %v", err)
	}

	enforcedPattern := regexp.MustCompile(`(?s)\*\*Enforced by:\*\*(.*?)\n\n`)
	linkPattern := regexp.MustCompile(`\[([^\]]+)\]\(([^)]+\.go)\)`)

	for _, block := range enforcedPattern.FindAllStringSubmatch(string(data), -1) {
		for _, link := range linkPattern.FindAllStringSubmatch(block[1], -1) {
			if _, err := os.Stat(link[2]); err != nil {
				t.Errorf("CONVENTIONS.md cites %s as enforcing code, but the file does not exist", link[2])
			}
		}
	}
}

// deferredConventions returns the ids whose section declares State: Deferred.
func deferredConventions(doc string) map[string]bool {
	deferred := make(map[string]bool)

	sections := convHeadingPattern.FindAllStringSubmatchIndex(doc, -1)
	for i, loc := range sections {
		id := doc[loc[2]:loc[3]]
		end := len(doc)
		if i+1 < len(sections) {
			end = sections[i+1][0]
		}
		body := doc[loc[1]:end]
		// Match "State: Deferred" but not "Enforced + Deferred" oddities;
		// a section is deferred only if Deferred is the whole state.
		if regexp.MustCompile(`(?m)^\*\*State: Deferred\*\*`).MatchString(body) {
			deferred[id] = true
		}
	}
	return deferred
}

// Each convention must declare a state, so no entry can sit in the
// undocumented fourth state the ledger exists to eliminate: claimed, but
// neither enforced nor delegated.
func TestConventions_EveryDefinitionDeclaresState(t *testing.T) {
	data, err := os.ReadFile("CONVENTIONS.md")
	if err != nil {
		t.Fatalf("read CONVENTIONS.md: %v", err)
	}
	doc := string(data)

	statePattern := regexp.MustCompile(`(?m)^\*\*State: (Enforced|Convention|Deferred)`)

	sections := convHeadingPattern.FindAllStringSubmatchIndex(doc, -1)
	for i, loc := range sections {
		id := doc[loc[2]:loc[3]]
		end := len(doc)
		if i+1 < len(sections) {
			end = sections[i+1][0]
		}
		if !statePattern.MatchString(doc[loc[1]:end]) {
			t.Errorf("%s does not declare a '**State: ...**' line (Enforced, Convention or Deferred)", id)
		}
	}
}

// The demo page renders each convention's state. Flattening a compound
// state there — showing CONV-13 as plain "Enforced" when the ledger says
// "Enforced (ordering) + Convention (the budget)" — hides the half that is
// the reader's responsibility. That is the drift this check catches; the
// id-existence checks above cannot see it.
func TestConventions_DemoPageStatesMatchLedger(t *testing.T) {
	pagePath := filepath.Join("scaffold", "static", "index.html")
	page, err := os.ReadFile(pagePath)
	if err != nil {
		t.Skipf("demo page not present: %v", err)
	}

	doc, err := os.ReadFile("CONVENTIONS.md")
	if err != nil {
		t.Fatalf("read CONVENTIONS.md: %v", err)
	}

	// Ledger states, keyed by id.
	ledger := make(map[string]string)
	sections := convHeadingPattern.FindAllStringSubmatchIndex(string(doc), -1)
	statePattern := regexp.MustCompile(`(?m)^\*\*State: (.+?)\*\*`)
	for i, loc := range sections {
		id := string(doc)[loc[2]:loc[3]]
		end := len(doc)
		if i+1 < len(sections) {
			end = sections[i+1][0]
		}
		if m := statePattern.FindStringSubmatch(string(doc)[loc[1]:end]); m != nil {
			ledger[id] = strings.TrimSpace(m[1])
		}
	}

	// States the page advertises.
	pageStatePattern := regexp.MustCompile(`data-conv="(CONV-\d+)" data-state="([^"]*)"`)
	matches := pageStatePattern.FindAllStringSubmatch(string(page), -1)
	if len(matches) == 0 {
		t.Fatal("demo page advertises no convention states — expected data-conv/data-state attributes")
	}

	for _, m := range matches {
		id, shown := m[1], m[2]
		want, ok := ledger[id]
		if !ok {
			t.Errorf("demo page shows %s, which CONVENTIONS.md does not define", id)
			continue
		}
		if shown != want {
			t.Errorf("%s: demo page shows state %q, CONVENTIONS.md declares %q", id, shown, want)
		}
	}

	// Every convention should appear on the page, so none is quietly dropped.
	for id := range ledger {
		found := false
		for _, m := range matches {
			if m[1] == id {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is defined in CONVENTIONS.md but absent from the demo page", id)
		}
	}
}
