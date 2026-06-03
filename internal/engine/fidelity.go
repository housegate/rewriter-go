package engine

// FidelityStatus classifies how faithfully the engine round-trips one statement.
type FidelityStatus int

const (
	FidelityOK            FidelityStatus = iota // parses and round-trip is idempotent
	FidelityParseError                          // ParseOne failed
	FidelityGenerateError                       // Generate failed
	FidelityNonIdempotent                       // gen1 != gen2 (information lost/mangled)
)

func (s FidelityStatus) String() string {
	switch s {
	case FidelityOK:
		return "OK"
	case FidelityParseError:
		return "ParseError"
	case FidelityGenerateError:
		return "GenerateError"
	case FidelityNonIdempotent:
		return "NonIdempotent"
	}
	return "Unknown"
}

// FidelityResult is the per-statement measurement.
type FidelityResult struct {
	SQL    string
	Status FidelityStatus
	Gen1   string
	Gen2   string
	Err    string
}

// CheckFidelity measures whether the engine faithfully represents one statement:
// it parses, generates (gen1), re-parses gen1, generates again (gen2), and
// checks gen1 == gen2 (idempotent round-trip).
func CheckFidelity(e Engine, sql string) FidelityResult {
	r := FidelityResult{SQL: sql}
	a1, err := e.ParseOne(sql)
	if err != nil {
		r.Status, r.Err = FidelityParseError, err.Error()
		return r
	}
	g1, err := e.Generate(a1)
	if err != nil {
		r.Status, r.Err = FidelityGenerateError, err.Error()
		return r
	}
	r.Gen1 = g1
	a2, err := e.ParseOne(g1)
	if err != nil {
		r.Status, r.Err = FidelityParseError, err.Error()
		return r
	}
	g2, err := e.Generate(a2)
	if err != nil {
		r.Status, r.Err = FidelityGenerateError, err.Error()
		return r
	}
	r.Gen2 = g2
	if g1 != g2 {
		r.Status = FidelityNonIdempotent
		return r
	}
	r.Status = FidelityOK
	return r
}
