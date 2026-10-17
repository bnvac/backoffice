package app

import (
	"sort"
	"strings"
	"time"
)

// BuildCost is what a project costs to make: the parts at what they were
// bought for, the filament its prints used, and the printer time. It is only
// as good as the prices recorded, so it says which lines it could not price
// rather than quietly counting them as free.
type BuildCost struct {
	Parts       []Money // per currency; never summed across currencies
	PartsPriced int
	Unpriced    []string // part lines with no known price

	Grams         float64
	FilamentCost  float64 // from spools whose price is known
	UnpricedGrams float64 // grams from spools without a price
	PrintSeconds  float64
	Prints        int
}

type Money struct {
	Amount   float64
	Currency string
}

func (m Money) String() string { return formatMoney(m.Amount, m.Currency) }

func (c BuildCost) Any() bool {
	return len(c.Parts) > 0 || c.Grams > 0 || c.PrintSeconds > 0 || len(c.Unpriced) > 0
}

func (c BuildCost) PrintTime() string {
	if c.PrintSeconds <= 0 {
		return ""
	}
	return roughDuration(time.Duration(c.PrintSeconds * float64(time.Second)))
}

// Total is the headline when everything is in one currency: parts plus
// filament. Spool prices carry no currency, so they are taken to be in the
// same one as the parts.
func (c BuildCost) Total() (Money, bool) {
	switch len(c.Parts) {
	case 0:
		if c.FilamentCost > 0 {
			return Money{c.FilamentCost, "USD"}, true
		}
		return Money{}, false
	case 1:
		return Money{c.Parts[0].Amount + c.FilamentCost, c.Parts[0].Currency}, true
	}
	return Money{}, false
}

// HasTotal and TotalLabel exist because templates cannot take two results.
func (c BuildCost) HasTotal() bool { _, ok := c.Total(); return ok }
func (c BuildCost) TotalLabel() string {
	m, _ := c.Total()
	return m.String()
}
func (c BuildCost) FilamentLabel() string {
	cur := "USD"
	if len(c.Parts) == 1 {
		cur = c.Parts[0].Currency
	}
	return formatMoney(c.FilamentCost, cur)
}

// CostOf works out a project's cost from its parts and the prints sent for it.
// A print marked as not printed does not count.
func CostOf(p Project, jobs []PrintJob, spools map[int64]Spool) BuildCost {
	var c BuildCost
	byCur := map[string]float64{}
	for _, part := range p.Parts {
		qty := part.Quantity
		if qty <= 0 {
			qty = 1
		}
		if part.Item == nil {
			if part.Sub == nil {
				c.Unpriced = append(c.Unpriced, part.Label())
			}
			continue
		}
		best := part.Item.Best()
		if best == nil || best.Amount <= 0 {
			c.Unpriced = append(c.Unpriced, part.Label())
			continue
		}
		cur := strings.ToUpper(best.Currency)
		if cur == "" {
			cur = "USD"
		}
		byCur[cur] += best.Amount * float64(qty)
		c.PartsPriced++
	}
	for cur, amt := range byCur {
		c.Parts = append(c.Parts, Money{amt, cur})
	}
	sort.Slice(c.Parts, func(i, j int) bool { return c.Parts[i].Currency < c.Parts[j].Currency })

	for _, j := range jobs {
		if !j.OK() || j.Returned() {
			continue
		}
		c.Prints++
		c.PrintSeconds += j.EstSeconds
		c.Grams += j.Grams
		if j.SpoolID == nil {
			continue
		}
		if sp, ok := spools[*j.SpoolID]; ok && sp.PerGram() > 0 {
			c.FilamentCost += j.Grams * sp.PerGram()
		} else {
			c.UnpricedGrams += j.Grams
		}
	}
	return c
}
