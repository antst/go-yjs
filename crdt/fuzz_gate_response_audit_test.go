package crdt

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

func validateFuzzSingleBase(rec fuzzSingleRec) error {
	stringsRequired := []struct {
		name  string
		value string
	}{
		{"updateV1", rec.UpdateV1},
		{"updateV2", rec.UpdateV2},
		{"state", rec.State},
		{"textDelta", rec.TextDelta},
		{"obfuscatedV1", rec.ObfuscatedV1},
		{"obfuscatedV2", rec.ObfuscatedV2},
	}
	for _, field := range stringsRequired {
		if field.value == "" {
			return fmt.Errorf("seed %d: single corpus lacks %s; that direction-A cell would compare NOTHING",
				rec.Seed, field.name)
		}
	}
	pointersRequired := []struct {
		name  string
		value any
	}{
		{"decodedStructs", rec.DecodedStructs},
		{"decodedStructsV2", rec.DecodedStructsV2},
		{"decodedDs", rec.DecodedDs},
		{"dsEqualAcrossFormats", rec.DsEqualAcrossFormats},
		{"dsEqualAfterExtraClient", rec.DsEqualAfterExtraClient},
		{"logTypeChildren", rec.LogTypeChildren},
		{"logTypeDeleted", rec.LogTypeDeleted},
		{"xmlString", rec.XMLString},
	}
	for _, field := range pointersRequired {
		if reflect.ValueOf(field.value).IsNil() {
			return fmt.Errorf("seed %d: single corpus lacks %s; that direction-A cell would compare NOTHING",
				rec.Seed, field.name)
		}
	}
	if *rec.DecodedDs == "" {
		return fmt.Errorf("seed %d: single corpus has empty decodedDs; that direction-A cell would compare NOTHING", rec.Seed)
	}
	return nil
}

func validateFuzzConcurrentBase(rec fuzzConcRec) error {
	if rec.JsDiverged {
		if rec.S1 == "" || rec.S2 == "" {
			return fmt.Errorf("seed %d: divergent reference record lacks one of s1/s2", rec.Seed)
		}
		return nil
	}
	stringsRequired := []struct {
		name  string
		value string
	}{
		{"baseV1", rec.BaseV1},
		{"baseV2", rec.BaseV2},
		{"u1v1", rec.U1V1},
		{"u2v1", rec.U2V1},
		{"u1v2", rec.U1V2},
		{"u2v2", rec.U2V2},
		{"full1V2", rec.Full1V2},
		{"full2V2", rec.Full2V2},
		{"state", rec.State},
		{"textDelta", rec.TextDelta},
	}
	for _, field := range stringsRequired {
		if field.value == "" {
			return fmt.Errorf("seed %d: concurrent corpus lacks %s; that direction-A cell would compare NOTHING",
				rec.Seed, field.name)
		}
	}
	return nil
}

func validateFuzzStrictSnapshot(rec fuzzSingleRec) error {
	stringsRequired := []struct {
		name  string
		value *string
	}{
		{"snapDocV1", rec.SnapDocV1},
		{"snapshotV1", rec.SnapshotV1},
		{"snapshotV2", rec.SnapshotV2},
		{"restoredState", rec.RestoredState},
		{"ychangeDocV1", rec.YChangeDocV1},
		{"ychangeEarlySnapV1", rec.YChangeEarlySnapV1},
		{"ychangeLateSnapV1", rec.YChangeLateSnapV1},
		{"ychangeDelta", rec.YChangeDelta},
		{"mapSnapshotAll", rec.MapSnapshotAll},
		{"snapLaterUpdateV1", rec.SnapLaterUpdateV1},
	}
	for _, field := range stringsRequired {
		if field.value == nil || *field.value == "" {
			return fmt.Errorf("seed %d: STRICT_SNAPSHOT corpus lacks non-empty %s; that direction-A cell would compare NOTHING",
				rec.Seed, field.name)
		}
	}
	if rec.SnapContainsSelf == nil || rec.SnapContainsLater == nil {
		return fmt.Errorf("seed %d: STRICT_SNAPSHOT corpus lacks snapshot containment fields", rec.Seed)
	}
	return nil
}

func validateFuzzStrictCells(rec fuzzSingleRec, strictXML, strictGC, strictSnapshot, strictSubdocs bool) error {
	if strictXML && rec.XMLString == nil {
		return fmt.Errorf("seed %d: STRICT_XML corpus lacks xmlString", rec.Seed)
	}
	if strictGC && (rec.PostGcState == nil || *rec.PostGcState == "") {
		return fmt.Errorf("seed %d: STRICT_GC corpus lacks non-empty postGcState", rec.Seed)
	}
	if strictSnapshot {
		if err := validateFuzzStrictSnapshot(rec); err != nil {
			return err
		}
	}
	if strictSubdocs && (rec.SubdocUpdateV1 == nil || *rec.SubdocUpdateV1 == "" || len(rec.SubdocGuids) == 0) {
		return fmt.Errorf("seed %d: STRICT_SUBDOCS corpus lacks update bytes or guid values", rec.Seed)
	}
	return nil
}

// TestDirectionAResponseSchemaIsConsumed mechanizes the audit that found SnapshotV1 declared and
// emitted but never read. The generator and both Go record types must expose the same fields, and
// every Go field must have at least one semantic read beyond its declaration and any nil/empty
// presence guard. A field that is merely required but never compared is still a hollow cell.
func TestDirectionAResponseSchemaIsConsumed(t *testing.T) {
	js, err := os.ReadFile(repoPath(t, "fuzz/generate.js"))
	if err != nil {
		t.Fatal(err)
	}
	source := string(js)
	single := sourceBetween(t, source, "function genSingle", "function genConcurrent")
	concurrent := sourceBetween(t, source, "function genConcurrent", "const gen =")

	singleEmitted := jsRecordFields(t, single, true)
	concurrentEmitted := jsRecordFields(t, concurrent, false)
	assertResponseSchema(t, "single", reflect.TypeOf(fuzzSingleRec{}), singleEmitted)
	assertResponseSchema(t, "concurrent", reflect.TypeOf(fuzzConcRec{}), concurrentEmitted)

	// Located by the function it audits rather than by filename.
	_, file := parseTestFileDeclaring(t, "TestFuzzGate")
	presenceOnly := make(map[*ast.SelectorExpr]bool)
	ast.Inspect(file, func(node ast.Node) bool {
		binary, ok := node.(*ast.BinaryExpr)
		if !ok || (binary.Op != token.EQL && binary.Op != token.NEQ) {
			return true
		}
		if selector, ok := binary.X.(*ast.SelectorExpr); ok && isEmptyResponseSentinel(binary.Y) {
			presenceOnly[selector] = true
		}
		if selector, ok := binary.Y.(*ast.SelectorExpr); ok && isEmptyResponseSentinel(binary.X) {
			presenceOnly[selector] = true
		}
		return true
	})
	semanticReads := make(map[string]int)
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if ok && !presenceOnly[selector] {
			semanticReads[selector.Sel.Name]++
		}
		return true
	})
	// S1 and S2 are the only diagnostic response fields: their semantic consumption is the
	// JsDiverged failure report, not a Go/reference comparison. They deliberately remain subject
	// to the same no-presence-only rule; do not turn this into a general exemption for diagnostics.
	for _, typ := range []reflect.Type{reflect.TypeOf(fuzzSingleRec{}), reflect.TypeOf(fuzzConcRec{})} {
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			if semanticReads[field.Name] == 0 {
				t.Errorf("%s.%s (%s) is declared but has no read beyond a nil/empty presence check; its direction-A cell compares NOTHING",
					typ.Name(), field.Name, field.Tag.Get("json"))
			}
		}
	}
}

func TestDirectionARequiredCellsCannotGoEmpty(t *testing.T) {
	one := 1
	yes := true
	nonempty := "x"
	empty := ""
	single := fuzzSingleRec{
		Seed: 1, Ops: 1,
		UpdateV1: "x", UpdateV2: "x", State: "{}", TextDelta: "[]",
		ObfuscatedV1: "x", ObfuscatedV2: "x",
		DecodedStructs: &one, DecodedStructsV2: &one, DecodedDs: &nonempty,
		DsEqualAcrossFormats: &yes, DsEqualAfterExtraClient: &yes,
		LogTypeChildren: &one, LogTypeDeleted: &one, XMLString: &empty,
		PostGcState: &nonempty, SnapDocV1: &nonempty, SnapshotV1: &nonempty,
		SnapshotV2: &nonempty, RestoredState: &nonempty, YChangeDocV1: &nonempty,
		YChangeEarlySnapV1: &nonempty, YChangeLateSnapV1: &nonempty, YChangeDelta: &nonempty,
		MapSnapshotAll: &nonempty, SnapContainsSelf: &yes, SnapContainsLater: &yes,
		SnapLaterUpdateV1: &nonempty, SubdocUpdateV1: &nonempty, SubdocGuids: []string{"g"},
	}
	if err := validateFuzzSingleBase(single); err != nil {
		t.Fatalf("complete single base rejected: %v", err)
	}
	baseFields := []string{
		"UpdateV1", "UpdateV2", "State", "TextDelta", "ObfuscatedV1", "ObfuscatedV2",
		"DecodedStructs", "DecodedStructsV2", "DecodedDs", "DsEqualAcrossFormats",
		"DsEqualAfterExtraClient", "LogTypeChildren", "LogTypeDeleted", "XMLString",
	}
	for _, name := range baseFields {
		broken := single
		field := reflect.ValueOf(&broken).Elem().FieldByName(name)
		field.Set(reflect.Zero(field.Type()))
		if err := validateFuzzSingleBase(broken); err == nil {
			t.Errorf("empty single base field %s was accepted", name)
		}
	}

	if err := validateFuzzStrictCells(single, true, true, true, true); err != nil {
		t.Fatalf("complete strict cells rejected: %v", err)
	}
	strictFields := []string{
		"PostGcState", "SnapDocV1", "SnapshotV1", "SnapshotV2", "RestoredState",
		"YChangeDocV1", "YChangeEarlySnapV1", "YChangeLateSnapV1", "YChangeDelta",
		"MapSnapshotAll", "SnapContainsSelf", "SnapContainsLater", "SnapLaterUpdateV1",
		"SubdocUpdateV1", "SubdocGuids",
	}
	for _, name := range strictFields {
		broken := single
		field := reflect.ValueOf(&broken).Elem().FieldByName(name)
		field.Set(reflect.Zero(field.Type()))
		if err := validateFuzzStrictCells(broken, true, true, true, true); err == nil {
			t.Errorf("empty strict field %s was accepted", name)
		}
	}

	concurrent := fuzzConcRec{
		Seed: 1, Ops: 1, BaseV1: "x", BaseV2: "x", U1V1: "x", U2V1: "x",
		U1V2: "x", U2V2: "x", Full1V2: "x", Full2V2: "x", State: "{}", TextDelta: "[]",
	}
	if err := validateFuzzConcurrentBase(concurrent); err != nil {
		t.Fatalf("complete concurrent base rejected: %v", err)
	}
	for _, name := range []string{"BaseV1", "BaseV2", "U1V1", "U2V1", "U1V2", "U2V2", "Full1V2", "Full2V2", "State", "TextDelta"} {
		broken := concurrent
		field := reflect.ValueOf(&broken).Elem().FieldByName(name)
		field.SetString("")
		if err := validateFuzzConcurrentBase(broken); err == nil {
			t.Errorf("empty concurrent field %s was accepted", name)
		}
	}
}

func isEmptyResponseSentinel(expr ast.Expr) bool {
	switch value := expr.(type) {
	case *ast.Ident:
		return value.Name == "nil"
	case *ast.BasicLit:
		return value.Kind == token.STRING && value.Value == `""`
	default:
		return false
	}
}

func sourceBetween(t *testing.T, source, start, end string) string {
	t.Helper()
	begin := strings.Index(source, start)
	if begin < 0 {
		t.Fatalf("generator marker %q not found", start)
	}
	finish := strings.Index(source[begin:], end)
	if finish < 0 {
		t.Fatalf("generator marker %q not found after %q", end, start)
	}
	return source[begin : begin+finish]
}

func jsRecordFields(t *testing.T, source string, single bool) map[string]bool {
	t.Helper()
	fields := make(map[string]bool)
	if single {
		start := strings.Index(source, "const rec = {")
		if start < 0 {
			t.Fatal("single generator record literal not found")
		}
		start += len("const rec = {")
		end := strings.Index(source[start:], "\n  };")
		if end < 0 {
			t.Fatal("single generator record literal end not found")
		}
		addJSObjectFields(fields, source[start:start+end])
		assignment := regexp.MustCompile(`rec\.([A-Za-z][A-Za-z0-9]*)\s*=`)
		for _, match := range assignment.FindAllStringSubmatch(source, -1) {
			fields[match[1]] = true
		}
		return fields
	}

	returns := regexp.MustCompile(`(?s)return\s*\{(.*?)\};`)
	for _, match := range returns.FindAllStringSubmatch(source, -1) {
		addJSObjectFields(fields, match[1])
	}
	return fields
}

func addJSObjectFields(fields map[string]bool, body string) {
	var uncommented strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if at := strings.Index(line, "//"); at >= 0 {
			line = line[:at]
		}
		uncommented.WriteString(line)
		uncommented.WriteByte('\n')
	}
	identifier := regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]*$`)
	for _, part := range strings.Split(uncommented.String(), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name := part
		if colon := strings.IndexByte(name, ':'); colon >= 0 {
			name = name[:colon]
		}
		name = strings.TrimSpace(name)
		if identifier.MatchString(name) {
			fields[name] = true
		}
	}
}

func assertResponseSchema(t *testing.T, mode string, typ reflect.Type, emitted map[string]bool) {
	t.Helper()
	declared := make(map[string]bool)
	for i := 0; i < typ.NumField(); i++ {
		name := strings.Split(typ.Field(i).Tag.Get("json"), ",")[0]
		if name != "" && name != "-" {
			declared[name] = true
		}
	}
	for name := range emitted {
		if !declared[name] {
			t.Errorf("%s generator emits %q but %s does not declare it; encoding/json would ignore the cell",
				mode, name, typ.Name())
		}
	}
	for name := range declared {
		if !emitted[name] {
			t.Errorf("%s declares %q but the %s generator never emits it", typ.Name(), name, mode)
		}
	}
}
