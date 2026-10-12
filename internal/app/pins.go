package app

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A project's pin budget: does the board you picked actually have enough legs
// for everything you have hung off it, and will any two of those things fight
// each other?
//
// The demand comes from the interface vocabulary already recorded against each
// part. The capacity comes from the controller's own specifications, and when
// the controller does not say, the budget says it does not know rather than
// inventing a number.

// pinCost is what one device of a given interface asks of the controller.
// Shared buses cost their lines once no matter how many devices hang off them,
// plus whatever each device needs on top -- a chip select, usually.
var pinCost = map[string]struct {
	Shared  int // lines the whole bus shares
	PerUnit int // lines each device needs of its own
	Note    string
}{
	"I2C":               {Shared: 2, PerUnit: 0, Note: "SDA + SCL, shared"},
	"Qwiic / STEMMA QT": {Shared: 2, PerUnit: 0, Note: "I2C on a connector, shared"},
	"SPI":               {Shared: 3, PerUnit: 1, Note: "SCK/MOSI/MISO shared, one CS each"},
	"I2S":               {Shared: 3, PerUnit: 0, Note: "BCLK/LRCLK/DATA, shared"},
	"1-Wire":            {Shared: 1, PerUnit: 0, Note: "one line, shared"},
	"CAN":               {Shared: 2, PerUnit: 0, Note: "TX + RX to the transceiver"},
	"UART":              {Shared: 0, PerUnit: 2, Note: "TX + RX, one pair each"},
	"PWM":               {Shared: 0, PerUnit: 1, Note: "one timer channel each"},
	"ADC":               {Shared: 0, PerUnit: 1, Note: "one analogue input each"},
	"DAC":               {Shared: 0, PerUnit: 1, Note: "one analogue output each"},
	"GPIO":              {Shared: 0, PerUnit: 1, Note: "one general-purpose pin each"},
}

// capacitySpecs are the specification names a pin count might be written under.
var capacitySpecs = []string{"gpio", "usable gpio", "io pins", "i/o pins", "pins", "digital pins", "gpio pins"}

// BusUse is one shared bus and everything on it.
type BusUse struct {
	Name    string
	Devices []string
	Pins    int
	Note    string
}

// PinDemand is one part's private claim on the controller's pins.
type PinDemand struct {
	Part   string
	ItemID int64
	Pins   int
	Detail string
}

// PinBudget is the whole answer for one project.
type PinBudget struct {
	Controller   *Item
	Capacity     int    // 0 when the controller does not say
	CapacityFrom string // which specification the figure came from
	Buses        []BusUse
	Demands      []PinDemand
	Total        int
	Conflicts    []string
	Notes        []string
}

// Known reports whether there is anything worth showing.
func (b PinBudget) Known() bool { return b.Controller != nil && (b.Total > 0 || len(b.Conflicts) > 0) }

// Spare is how many pins are left. Only meaningful when Capacity is known.
func (b PinBudget) Spare() int { return b.Capacity - b.Total }

// Over means the project asks for more pins than the board has.
func (b PinBudget) Over() bool { return b.Capacity > 0 && b.Total > b.Capacity }

// Percent is how full the board is, for the bar on the project page.
func (b PinBudget) Percent() int {
	if b.Capacity <= 0 {
		return 0
	}
	p := b.Total * 100 / b.Capacity
	if p > 100 {
		p = 100
	}
	return p
}

// i2cAddress pulls a 0x-style address out of a specification value. Parts often
// list several ("0x76 / 0x77" for a selectable address line), and all of them
// are returned so a genuine clash is not reported when a jumper would fix it.
var addressPattern = regexp.MustCompile(`0[xX][0-9a-fA-F]{2}`)

func i2cAddresses(it *Item) []string {
	var out []string
	for _, sp := range it.Specs {
		name := strings.ToLower(sp.Name)
		if !strings.Contains(name, "address") && !strings.Contains(name, "addr") {
			continue
		}
		for _, m := range addressPattern.FindAllString(sp.Value, -1) {
			out = append(out, strings.ToLower(m))
		}
	}
	return out
}

// pinCapacity reads a usable pin count out of the controller's specifications.
func pinCapacity(it *Item) (int, string) {
	for _, want := range capacitySpecs {
		for _, sp := range it.Specs {
			if strings.ToLower(strings.TrimSpace(sp.Name)) != want {
				continue
			}
			if n := firstInt(sp.Value); n > 0 {
				return n, sp.Name
			}
		}
	}
	// A looser pass, for "Usable GPIO pins" and similar phrasings.
	for _, sp := range it.Specs {
		name := strings.ToLower(sp.Name)
		if strings.Contains(name, "gpio") || strings.Contains(name, "io pin") || strings.Contains(name, "pins") {
			if n := firstInt(sp.Value); n > 0 {
				return n, sp.Name
			}
		}
	}
	return 0, ""
}

var intPattern = regexp.MustCompile(`\d+`)

func firstInt(s string) int {
	if m := intPattern.FindString(s); m != "" {
		n, _ := strconv.Atoi(m)
		return n
	}
	return 0
}

// BudgetPins works out what a project asks of its controller.
func BudgetPins(p Project) PinBudget {
	var b PinBudget
	if p.Controller == nil {
		return b
	}
	b.Controller = p.Controller
	b.Capacity, b.CapacityFrom = pinCapacity(p.Controller)

	shared := map[string]*BusUse{}
	var busOrder []string
	addresses := map[string][]string{} // i2c address -> parts using it

	for _, part := range p.Parts {
		it := part.Item
		if it == nil || it.ID == p.Controller.ID {
			continue
		}
		units := part.Quantity
		if units < 1 {
			units = 1
		}

		private, detail := 0, []string{}
		for _, io := range it.Interfaces {
			cost, ok := pinCost[io]
			if !ok {
				continue
			}
			if cost.Shared > 0 {
				bus, seen := shared[io]
				if !seen {
					bus = &BusUse{Name: io, Pins: cost.Shared, Note: cost.Note}
					shared[io] = bus
					busOrder = append(busOrder, io)
				}
				bus.Devices = append(bus.Devices, fmt.Sprintf("%s ×%d", it.Name, units))
			}
			if cost.PerUnit > 0 {
				private += cost.PerUnit * units
				detail = append(detail, fmt.Sprintf("%d for %s", cost.PerUnit*units, io))
			}
		}

		// An I2C device with a fixed address can only appear once on the bus.
		for _, addr := range i2cAddresses(it) {
			label := it.Name
			if units > 1 {
				label = fmt.Sprintf("%s ×%d", it.Name, units)
			}
			addresses[addr] = append(addresses[addr], label)
		}

		if private > 0 {
			b.Demands = append(b.Demands, PinDemand{
				Part: it.Name, ItemID: it.ID, Pins: private,
				Detail: strings.Join(detail, ", "),
			})
		}
	}

	for _, name := range busOrder {
		bus := shared[name]
		b.Buses = append(b.Buses, *bus)
		b.Total += bus.Pins
	}
	sort.SliceStable(b.Demands, func(i, j int) bool { return b.Demands[i].Pins > b.Demands[j].Pins })
	for _, d := range b.Demands {
		b.Total += d.Pins
	}

	b.Conflicts = append(b.Conflicts, addressConflicts(addresses)...)
	b.Conflicts = append(b.Conflicts, levelConflicts(p)...)

	switch {
	case b.Capacity == 0:
		b.Notes = append(b.Notes,
			fmt.Sprintf("%s does not list a pin count — add a %q specification to it and this becomes a real budget",
				p.Controller.Name, "GPIO"))
	case b.Over():
		b.Conflicts = append(b.Conflicts,
			fmt.Sprintf("%d pins needed but %s has %d — you need an IO expander, or to move something onto a shared bus",
				b.Total, p.Controller.Name, b.Capacity))
	case b.Spare() <= 2:
		b.Notes = append(b.Notes, fmt.Sprintf("only %s spare — no room for a status LED or a button",
			plural(b.Spare(), "pin")))
	}
	return b
}

// addressConflicts reports I2C addresses claimed by more than one part. A part
// listing several addresses is assumed to be selectable and is not counted as a
// clash on its own.
func addressConflicts(addresses map[string][]string) []string {
	var addrs []string
	for a := range addresses {
		addrs = append(addrs, a)
	}
	sort.Strings(addrs)

	var out []string
	for _, a := range addrs {
		parts := dedupe(addresses[a])
		if len(parts) > 1 {
			out = append(out, fmt.Sprintf("I2C address %s is claimed by %s — only one of them can be on the bus unless its address is jumper-selectable",
				a, strings.Join(parts, " and ")))
		}
	}
	return out
}

// levelConflicts flags 5 V parts hung off a 3.3 V controller, which is the
// mistake that quietly kills an ESP32.
func levelConflicts(p Project) []string {
	if p.Controller == nil {
		return nil
	}
	controller3v3 := hasInterface(p.Controller, "3V3 logic")
	controller5v := hasInterface(p.Controller, "5V logic")
	if !controller3v3 && !controller5v {
		return nil
	}

	var mismatched []string
	for _, part := range p.Parts {
		it := part.Item
		if it == nil || it.ID == p.Controller.ID {
			continue
		}
		if controller3v3 && hasInterface(it, "5V logic") && !hasInterface(it, "3V3 logic") {
			mismatched = append(mismatched, it.Name)
		}
		if controller5v && hasInterface(it, "3V3 logic") && !hasInterface(it, "5V logic") {
			mismatched = append(mismatched, it.Name)
		}
	}
	if len(mismatched) == 0 {
		return nil
	}
	level := "3.3 V"
	if controller5v {
		level = "5 V"
	}
	names := dedupe(mismatched)
	verb := "does not"
	if len(names) > 1 {
		verb = "do not"
	}
	return []string{fmt.Sprintf("%s runs at %s but %s %s — you need a level shifter between them",
		p.Controller.Name, level, strings.Join(names, ", "), verb)}
}

func hasInterface(it *Item, name string) bool {
	for _, i := range it.Interfaces {
		if i == name {
			return true
		}
	}
	return false
}

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
