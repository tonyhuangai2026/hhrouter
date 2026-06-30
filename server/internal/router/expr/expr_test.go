package expr

import "testing"

func TestCompile_EmptyAlwaysMatches(t *testing.T) {
	p, err := Compile("   ")
	if err != nil {
		t.Fatalf("empty compile: %v", err)
	}
	if !p.Eval(Vars{}) {
		t.Error("empty expression should always match")
	}
	if len(p.Refs) != 0 {
		t.Errorf("empty expr refs = %v, want none", p.Refs)
	}
}

func TestEval_NumericAndLogic(t *testing.T) {
	cases := []struct {
		src  string
		w, t int
		want bool
	}{
		{"w == 1", 1, 0, true},
		{"w == 1", 0, 0, false},
		{"t > 500", 0, 600, true},
		{"t > 500", 0, 500, false},
		{"t >= 500", 0, 500, true},
		{"w == 1 && t > 500", 1, 600, true},
		{"w == 1 && t > 500", 1, 100, false},
		{"w == 1 || t > 500", 0, 600, true},
		{"w == 1 || t > 500", 0, 100, false},
		{"!(w == 1)", 0, 0, true},
		{"w != 0", 1, 0, true},
		{"(w == 1 || t < 100) && t != 5", 0, 50, true},
	}
	for _, c := range cases {
		p, err := Compile(c.src)
		if err != nil {
			t.Fatalf("compile %q: %v", c.src, err)
		}
		got := p.Eval(Vars{Int: map[string]int{"w": c.w, "t": c.t}})
		if got != c.want {
			t.Errorf("%q with w=%d t=%d = %v, want %v", c.src, c.w, c.t, got, c.want)
		}
	}
}

func TestEval_StringVars(t *testing.T) {
	p, err := Compile(`group == "vip" && model != "gpt-4o-mini"`)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !p.Eval(Vars{Str: map[string]string{"group": "vip", "model": "claude-opus-4-8"}}) {
		t.Error("vip + non-mini should match")
	}
	if p.Eval(Vars{Str: map[string]string{"group": "free", "model": "x"}}) {
		t.Error("free group should not match")
	}
}

func TestCompile_Refs(t *testing.T) {
	p, _ := Compile(`w == 1 && group == "vip"`)
	if !p.References("w") || !p.References("group") {
		t.Errorf("refs = %v, want w+group", p.Refs)
	}
	if p.References("t") {
		t.Error("should not reference t")
	}
}

func TestCompile_Errors(t *testing.T) {
	bad := []string{
		"w = 1",         // single = (assignment, not allowed)
		"foo == 1",      // unknown variable
		"w == 1 &",      // dangling &
		"w == ",         // missing rhs
		"(w == 1",       // unclosed paren
		"w == 1 t == 2", // two expressions, no operator
		`group == 5`,    // type mismatch string vs int
		"w + 1",         // no arithmetic
		"w",             // bare int var is not a boolean condition
		`"hi"`,          // bare string is not boolean
		"w == 1 && t",   // rhs of && not boolean
		`true && group`, // group is string, not boolean
	}
	for _, src := range bad {
		if _, err := Compile(src); err == nil {
			t.Errorf("expected error compiling %q, got nil", src)
		}
	}
}

func TestCompile_BoolLiterals(t *testing.T) {
	p, err := Compile("true || w == 1")
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	if !p.Eval(Vars{}) {
		t.Error("true || ... should match")
	}
}
