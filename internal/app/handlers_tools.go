package app

import (
	"net/http"
	"strings"
)

type toolsData struct {
	Calculators []Calculator
	Active      string
	Inputs      CalcInputs
	Result      *CalcResult
	Error       string
}

// Values is what the template needs to keep the form filled in after a submit.
func (d toolsData) Values(key string) string { return d.Inputs[key] }

// IsActive says which calculator's result panel to show open.
func (d toolsData) IsActive(key string) bool { return d.Active == key }

// handleTools runs the bench calculators. Every calculator is a plain GET form:
// the answer is in the URL, so it can be bookmarked, shared, or reopened later
// with the same numbers, and none of it needs JavaScript.
func (a *App) handleTools(w http.ResponseWriter, r *http.Request) {
	data := toolsData{Calculators: Calculators, Inputs: CalcInputs{}}

	key := strings.TrimSpace(r.URL.Query().Get("calc"))
	calc := CalculatorByKey(key)
	if calc != nil {
		data.Active = calc.Key
		for k, v := range r.URL.Query() {
			if len(v) > 0 {
				data.Inputs[k] = strings.TrimSpace(v[0])
			}
		}
		result, err := calc.Run(data.Inputs)
		if err != nil {
			data.Error = err.Error()
		} else {
			// The part of this a website calculator cannot do: check the answer
			// against what is actually on the shelf.
			if result.Target > 0 && result.TargetUnit != "" {
				if items, err := a.store.ListItems(Query{}); err == nil {
					result.Matches = MatchStock(items, result.Target, result.TargetUnit, 10, 6)
				}
			}
			data.Result = &result
		}
	}
	a.render(w, r, "tools.html", "Calculators", data)
}
