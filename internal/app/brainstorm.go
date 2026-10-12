package app

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// The idea board: what could you build out of what is already in the drawers?
//
// There are two suggesters here and they answer the same question differently.
// The one that reads the shelf directly is rules and recipes -- it can only
// propose things it can name the parts for, so every suggestion is buildable
// tonight, and it needs no network and no model. The one that asks a model is
// better at the leap you would not have thought of, and worse at knowing what
// you own, so it is given the shelf and the workshop in the prompt and its
// answers are checked back against both before they are shown.
//
// Neither is allowed to invent a part you do not have without saying so.

// Idea is one entry on the board.
type Idea struct {
	ID        int64
	Title     string
	Summary   string
	Detail    string
	Status    string // new | keeping | dropped | built
	Source    string // "your shelf" or the provider's name
	Enclosure string
	Effort    string
	ProjectID *int64
	CreatedAt time.Time

	Parts []IdeaPart
}

// IdeaPart is one thing the idea needs, and whether it is on the shelf.
type IdeaPart struct {
	ID       int64
	IdeaID   int64
	ItemID   *int64
	Name     string
	Quantity int
	Role     string
	Have     bool
}

func (i Idea) Planned() bool { return i.ProjectID != nil && *i.ProjectID > 0 }

func (i Idea) ProjectRef() int64 {
	if i.ProjectID == nil {
		return 0
	}
	return *i.ProjectID
}

// Shortfall is what you would have to buy.
func (i Idea) Shortfall() []IdeaPart {
	var out []IdeaPart
	for _, p := range i.Parts {
		if !p.Have {
			out = append(out, p)
		}
	}
	return out
}

func (i Idea) Buildable() bool { return len(i.Parts) > 0 && len(i.Shortfall()) == 0 }

func (i Idea) StatusLabel() string {
	switch i.Status {
	case "keeping":
		return "Keeping"
	case "dropped":
		return "Dropped"
	case "built":
		return "Built"
	}
	return "New"
}

// --- reading the shelf ------------------------------------------------------

// A part's role is what it does in a project, which is a different question
// from what category it was filed under. It is worked out from everything the
// item already records -- its category, the interfaces it speaks, its tags and
// its name -- because no single one of those is filled in reliably.

type roleRule struct {
	role  string
	words []string
}

// roleRules are checked in order, first match wins, so the specific roles come
// before the general ones: a "soil moisture sensor" is soil, not just sensor.
var roleRules = []roleRule{
	{"controller", []string{"esp32", "esp8266", "esp32-c3", "esp32-s3", "rp2040", "pico",
		"arduino", "nano", "uno", "mega", "teensy", "stm32", "blue pill", "attiny",
		"atmega", "nrf52", "microcontroller", "devkit", "dev board", "xiao", "d1 mini",
		"feather", "qt py", "particle", "wroom", "wrover"}},
	{"computer", []string{"raspberry pi", "pi zero",
		"orange pi", "banana pi", "rock pi", "odroid", "jetson", "beaglebone", "sbc",
		"mini pc", "thin client"}},
	{"camera", []string{"camera", "ov2640", "ov5640", "esp32-cam", "webcam", "imx"}},
	{"display", []string{"oled", "lcd", "tft", "e-ink", "eink", "epaper", "e-paper",
		"ssd1306", "st7789", "ili9341", "sh1106", "hd44780", "nextion", "matrix display",
		"seven segment", "7-segment", "display"}},
	{"led", []string{"ws2812", "neopixel", "sk6812", "apa102", "led strip", "addressable",
		"rgb led", "led ring", "led"}},
	{"radio", []string{"lora", "sx1276", "sx1262", "rfm95", "nrf24", "cc1101", "zigbee",
		"z-wave", "433mhz", "868mhz", "915mhz", "rf module", "xbee"}},
	{"network", []string{"ethernet", "w5500", "enc28j60", "poe", "rs485", "can transceiver",
		"mcp2515", "switch", "router", "access point"}},
	{"environment", []string{"bme280", "bme680", "bmp280", "dht11", "dht22", "sht30",
		"sht31", "sht40", "aht20", "ds18b20", "scd30", "scd40", "mh-z19", "co2",
		"pms5003", "particulate", "temperature", "humidity", "barometer", "air quality",
		"tmp35", "tmp36", "tmp37", "lm35", "lm34", "thermistor", "ntc", "thermocouple",
		"max6675", "max31855", "pt100", "pt1000", "tmp117", "si7021", "hdc1080"}},
	{"motion", []string{"pir", "hc-sr501", "radar", "ld2410", "rcwl", "accelerometer",
		"gyro", "imu", "mpu6050", "mpu9250", "bno055", "lsm6ds", "tilt", "vibration",
		"motion"}},
	{"distance", []string{"hc-sr04", "ultrasonic", "vl53l0x", "vl53l1x", "tof",
		"time of flight", "lidar", "rangefinder", "proximity"}},
	{"light", []string{"ldr", "photoresistor", "bh1750", "tsl2561", "veml", "uv sensor",
		"lux", "apds9960", "colour sensor", "color sensor"}},
	{"soil", []string{"soil moisture", "capacitive soil", "hygrometer", "water level",
		"rain sensor", "flow sensor", "yf-s201"}},
	{"current", []string{"ina219", "ina226", "acs712", "ct clamp", "current sensor",
		"energy meter", "pzem"}},
	{"scale", []string{"hx711", "load cell", "strain gauge"}},
	{"identity", []string{"rfid", "nfc", "pn532", "rc522", "fingerprint", "keypad",
		"card reader"}},
	{"position", []string{"gps", "neo-6m", "neo-8m", "gnss", "magnetometer", "compass"}},
	{"audio", []string{"microphone", "inmp441", "max9814", "i2s mic", "speaker", "buzzer",
		"max98357", "amplifier", "pam8403", "dfplayer", "audio"}},
	{"actuator", []string{"relay", "servo", "stepper", "nema17", "a4988", "drv8825",
		"tmc2209", "dc motor", "motor driver", "l298", "solenoid", "pump", "valve",
		"linear actuator", "fan", "mosfet module", "triac", "ssr"}},
	{"power", []string{"18650", "lipo", "li-ion", "battery", "tp4056", "charger",
		"buck", "boost", "regulator", "ams1117", "mt3608", "lm2596", "power bank",
		"solar panel", "psu", "power supply", "ups"}},
	{"storage", []string{"sd card", "microsd", "sd module", "eeprom", "flash module",
		"ssd", "hard drive", "nvme", "usb stick"}},
	{"clock", []string{"ds3231", "ds1307", "rtc", "real time clock"}},
	{"input", []string{"button", "switch module", "rotary encoder", "potentiometer",
		"joystick", "touch sensor", "ttp223", "limit switch"}},
}

// roleOf works out what a part would do in a project.
func roleOf(it Item) string {
	hay := strings.ToLower(strings.Join([]string{
		it.Name, it.Category, it.Value, it.PartNumber, tagWords(it),
	}, " "))
	for _, rule := range roleRules {
		for _, word := range rule.words {
			if strings.Contains(hay, word) {
				return rule.role
			}
		}
	}
	// Nothing matched by name. The interfaces an item speaks are the last
	// usable signal: a thing that speaks WiFi is almost certainly the brain.
	for _, io := range it.Interfaces {
		switch strings.ToLower(io) {
		case "wifi", "bluetooth", "ble":
			if strings.Contains(strings.ToLower(it.Category), "sensor") {
				return "environment"
			}
			return "controller"
		}
	}
	switch strings.ToLower(it.Category) {
	case "microcontroller", "mcu":
		return "controller"
	case "sbc", "computer":
		return "computer"
	case "sensor":
		return "environment"
	case "display":
		return "display"
	case "power":
		return "power"
	}
	return ""
}

func tagWords(it Item) string {
	var b strings.Builder
	for _, t := range it.Tags {
		b.WriteString(t.Name)
		b.WriteByte(' ')
	}
	return b.String()
}

// Shelf is the inventory arranged by what each part does, which is the shape
// both suggesters want.
type Shelf struct {
	ByRole map[string][]Item
	// Everything in stock, including the parts no role rule recognised. A
	// converter, a level shifter or a connector has no role in a recipe but is
	// still on the shelf, and answering "do you own one" needs the whole list.
	All   []Item
	Items int
	Roles []string
}

// ReadShelf sorts the inventory into roles, keeping only parts you have in
// stock -- a suggestion you cannot build tonight is not a suggestion.
func ReadShelf(items []Item) Shelf {
	s := Shelf{ByRole: map[string][]Item{}}
	for _, it := range items {
		if it.Available() < 1 {
			continue
		}
		s.All = append(s.All, it)
		role := roleOf(it)
		if role == "" {
			continue
		}
		s.ByRole[role] = append(s.ByRole[role], it)
		s.Items++
	}
	for role := range s.ByRole {
		s.Roles = append(s.Roles, role)
	}
	sort.Strings(s.Roles)
	return s
}

// Has reports whether any of the roles are on the shelf, returning the first
// part found so a suggestion can name it.
func (s Shelf) Has(roles ...string) (Item, bool) {
	for _, role := range roles {
		if list := s.ByRole[role]; len(list) > 0 {
			return list[0], true
		}
	}
	return Item{}, false
}

// Brain is the controller a project would be built around, preferring a
// microcontroller over a full computer because most of these are small.
func (s Shelf) Brain() (Item, bool) { return s.Has("controller", "computer") }

// --- the recipes ------------------------------------------------------------

// recipe is one thing worth building, described by the roles it needs.
type recipe struct {
	title     string
	summary   string
	needs     []string // roles, all required; each entry may offer alternatives
	effort    string
	enclosure string
}

// Each need is a group of interchangeable roles separated by "|". "controller"
// is implied by nearly all of them and stated anyway, because a recipe that
// cannot name its brain cannot be built.
var recipes = []recipe{
	{"Room climate monitor", "Read temperature and humidity and put them somewhere you can see, on a board that can reach the network.",
		[]string{"controller", "environment"}, "an evening", "a small wall box with vents over the sensor"},
	{"Desk dashboard", "Show whatever you care about on a screen that sits on the desk and never needs touching.",
		[]string{"controller", "display"}, "an evening", "an angled desk stand with the screen flush"},
	{"Network-controlled switch", "Put a relay on the network so something dumb can be turned on and off from anything.",
		[]string{"controller", "actuator"}, "an evening", "a sealed box — mains-adjacent wiring wants a lid that stays shut"},
	{"Motion-triggered light", "Turn a light on when somebody walks past and off again when they stop.",
		[]string{"controller", "motion", "led|actuator"}, "an evening", "a corner mount with the sensor dome clear"},
	{"Plant watering rig", "Watch the soil and run a pump when it dries out.",
		[]string{"controller", "soil", "actuator"}, "a weekend", "a splash-proof box, well away from the pot"},
	{"Energy monitor", "Measure what something actually draws, and keep the history.",
		[]string{"controller", "current"}, "a weekend", "a DIN-rail or sealed box near the meter"},
	{"Long-range remote sensor", "Put a sensor somewhere with no WiFi and send readings back over radio.",
		[]string{"controller", "radio", "environment|soil|distance"}, "a weekend", "a weatherproof box with a gland for the antenna"},
	{"Doorbell or access reader", "Read a card or a code and decide whether to let it through.",
		[]string{"controller", "identity"}, "a weekend", "a flush plate with the reader behind a thin wall"},
	{"Bench power meter", "Watch volts and amps on a display while you work.",
		[]string{"controller", "current", "display"}, "an evening", "a small case that sits at eye level on the bench"},
	{"Digital scale", "Turn a load cell into something that reads out in grams.",
		[]string{"controller", "scale", "display"}, "a weekend", "a flat platform with the cell mounted between two plates"},
	{"E-ink status panel", "A screen that only redraws when something changes and sips power the rest of the time.",
		[]string{"controller", "display"}, "a weekend", "a picture-frame style bezel"},
	{"Distance or level gauge", "Measure how far away something is — a bin, a tank, a parking space.",
		[]string{"controller", "distance"}, "an evening", "a lid mount pointing straight down"},
	{"Time-lapse or camera trap", "Take a picture on a timer or when something moves.",
		[]string{"controller", "camera"}, "a weekend", "a case with a lens hole and a tripod thread"},
	{"Sound-reactive light", "Make a strip of LEDs follow whatever is playing.",
		[]string{"controller", "audio", "led"}, "an evening", "a diffuser channel — bare LEDs look terrible"},
	{"Home file server", "A machine that keeps things and hands them back, on hardware you already have.",
		[]string{"computer", "storage"}, "a weekend", "a stacked drive frame with a fan duct"},
	{"GPS tracker or logger", "Record where something went, and how fast.",
		[]string{"controller", "position", "storage|radio"}, "a weekend", "a pocket-sized box with the antenna facing up"},
	{"Clock that is actually right", "A clock that keeps time through a power cut and never drifts.",
		[]string{"controller", "clock", "display"}, "an evening", "a plain desk case — this one is about the face"},
	{"Rotary control panel", "Knobs and buttons that do something useful to something else.",
		[]string{"controller", "input"}, "an evening", "a sloped panel with the encoders on the top face"},
}

// SuggestFromShelf proposes what could be built from the parts in stock.
//
// Every idea it returns names real items, because it will not emit a recipe
// whose roles are not all present. That is the trade: fewer suggestions than a
// model would give, and none of them fictional.
func SuggestFromShelf(items []Item, caps Capabilities, limit int) []Idea {
	shelf := ReadShelf(items)
	var out []Idea

	for _, r := range recipes {
		parts, ok := matchRecipe(shelf, r)
		if !ok {
			continue
		}
		idea := Idea{
			Title:     r.title,
			Summary:   r.summary,
			Source:    "your shelf",
			Effort:    r.effort,
			Status:    "new",
			Parts:     parts,
			Enclosure: enclosureAdvice(caps, r.enclosure),
		}
		idea.Detail = shelfReasoning(parts, caps)
		out = append(out, idea)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out
}

// analogueParts are the sensors that hand back a voltage rather than a number.
// It matters because it decides whether a given brain can read them at all.
var analogueParts = []string{
	"tmp35", "tmp36", "tmp37", "lm35", "lm34", "thermistor", "ntc",
	"ldr", "photoresistor", "potentiometer", "pot ", "flex sensor",
	"force sensitive", "fsr", "microphone module", "sound sensor",
	"gas sensor", "mq-2", "mq-3", "mq-4", "mq-7", "mq-135",
	"analog", "analogue",
}

// isAnalogue reports whether a part needs an analogue input to be read.
func isAnalogue(name string) bool {
	lower := " " + strings.ToLower(name) + " "
	for _, word := range analogueParts {
		if strings.Contains(lower, word) {
			// A soil probe sold as "capacitive" still outputs a voltage, but the
			// digital modules say so on the tin, so an explicit mention wins.
			if strings.Contains(lower, "digital") || strings.Contains(lower, "i2c") {
				return false
			}
			return true
		}
	}
	return false
}

// hasADC looks for something on the shelf that could read an analogue signal
// for a board that cannot do it itself.
func hasADC(shelf Shelf) (Item, bool) {
	for _, it := range shelf.All {
		lower := strings.ToLower(it.Name)
		for _, adc := range []string{"mcp3008", "mcp3208", "ads1115", "ads1015", "pcf8591", "adc"} {
			if strings.Contains(lower, adc) {
				return it, true
			}
		}
	}
	return Item{}, false
}

// matchRecipe finds one part for each role a recipe needs.
//
// A recipe that asks for a "controller" is happy with a single-board computer
// too: a Pi runs every one of these. Spelling that out here rather than in
// seventeen recipe strings keeps the recipes about the project rather than
// about the hardware, and it is the difference between a shelf full of Pis
// getting suggestions and getting nothing at all.
func matchRecipe(shelf Shelf, r recipe) ([]IdeaPart, bool) {
	var parts []IdeaPart
	used := map[int64]bool{}
	brainIsComputer := false

	for _, need := range r.needs {
		options := strings.Split(need, "|")
		if need == "controller" {
			options = append(options, "computer")
		}
		found := false
		for _, role := range options {
			for _, it := range shelf.ByRole[role] {
				if used[it.ID] {
					continue
				}
				id := it.ID
				if role == "computer" && need == "controller" {
					brainIsComputer = true
				}
				parts = append(parts, IdeaPart{
					ItemID: &id, Name: it.Name, Quantity: 1, Role: role, Have: true,
				})
				used[id] = true
				found = true
				break
			}
			if found {
				break
			}
		}
		if !found {
			return nil, false
		}
	}

	// A single-board computer has no analogue input. Suggesting it read a TMP35
	// without saying so would send somebody to the bench to find out the hard
	// way, so the converter is listed as a part like any other -- ticked off if
	// one is already on the shelf, and on the shopping list if not.
	if brainIsComputer && needsADC(parts) {
		if adc, ok := hasADC(shelf); ok {
			id := adc.ID
			parts = append(parts, IdeaPart{
				ItemID: &id, Name: adc.Name, Quantity: 1, Role: "adc", Have: true,
			})
		} else {
			parts = append(parts, IdeaPart{
				Name: "an ADC — MCP3008 or ADS1115", Quantity: 1, Role: "adc", Have: false,
			})
		}
	}
	return parts, true
}

// needsADC is true when any chosen part hands back a voltage.
func needsADC(parts []IdeaPart) bool {
	for _, p := range parts {
		if p.Role != "adc" && isAnalogue(p.Name) {
			return true
		}
	}
	return false
}

// shelfReasoning writes down why this was suggested, so the board is a record
// of reasoning rather than a list of assertions.
func shelfReasoning(parts []IdeaPart, caps Capabilities) string {
	var names []string
	for _, p := range parts {
		if p.Have {
			names = append(names, p.Name)
		}
	}
	text := "Suggested because you already have " + joinWords(names) + "."
	for _, p := range parts {
		if p.Role == "adc" {
			text += " A single-board computer has no analogue input, so the sensor" +
				" has to go through a converter rather than straight onto a pin."
			break
		}
	}
	switch {
	case caps.Print3D:
		text += " You can print the case for it."
	case caps.Drill:
		text += " No printer, but you can cut and drill a bought box."
	}
	if !caps.Solder {
		text += " Nothing here needs soldering if the parts are on breakout boards."
	}
	return text
}

func joinWords(list []string) string {
	switch len(list) {
	case 0:
		return "nothing"
	case 1:
		return list[0]
	case 2:
		return list[0] + " and " + list[1]
	}
	return strings.Join(list[:len(list)-1], ", ") + " and " + list[len(list)-1]
}

// --- enclosures -------------------------------------------------------------

// enclosureAdvice says how the box gets made, which depends entirely on what is
// in the workshop. Telling somebody with no printer to print a case is the kind
// of suggestion that makes a tool feel like it is not listening.
func enclosureAdvice(caps Capabilities, hint string) string {
	switch {
	case caps.Print3D && caps.Measure:
		text := "Print it: " + hint + "."
		if caps.BuildVolume.Known() {
			text += " Your bed is " + caps.BuildVolume.String() + "."
		}
		return text + " Measure the boards with your calipers before you model it — published dimensions are often the PCB, not the connectors."
	case caps.Print3D:
		text := "Print it: " + hint + "."
		if caps.BuildVolume.Known() {
			text += " Your bed is " + caps.BuildVolume.String() + "."
		}
		return text + " You have no calipers listed, so take dimensions from the datasheets and add a millimetre of slop."
	case caps.Laser:
		return "No printer, but you can laser cut it: a stacked flat-pack case out of 3 mm acrylic or ply. " + hint + "."
	case caps.Drill:
		return "No printer. Buy a project box a size larger than you think and cut the openings — " + hint + "."
	}
	return "No printer and no cutting tools listed. Look for a project box with the cutouts already in it, or a case sold for the board itself."
}

// EnclosurePlan is the answer to "how do I put this in a box", worked out for
// a specific set of parts rather than in general.
type EnclosurePlan struct {
	Method      string // print | laser | cut | buy
	Advice      string
	SearchTerms []string // what to look for on a model site
	Measure     []string // parts whose size nobody knows yet
	Volume      Volume
	CanSearch   bool
}

// PlanEnclosure decides how a project's box gets made and what to look up.
func PlanEnclosure(caps Capabilities, parts []Item) EnclosurePlan {
	plan := EnclosurePlan{Volume: caps.BuildVolume}
	switch {
	case caps.Print3D:
		plan.Method, plan.CanSearch = "print", true
	case caps.Laser:
		plan.Method = "laser"
	case caps.Drill:
		plan.Method = "cut"
	default:
		plan.Method = "buy"
	}
	plan.Advice = enclosureAdvice(caps, "a box sized around the largest board")

	for _, it := range parts {
		if plan.CanSearch {
			plan.SearchTerms = append(plan.SearchTerms, ModelSearchTerms(it)...)
		}
		// A part with no recorded size is one you cannot design around. Saying
		// so is more useful than guessing, and the answer is different
		// depending on whether there are calipers on the bench.
		if !hasSize(it) {
			plan.Measure = append(plan.Measure, it.Name)
		}
	}
	plan.SearchTerms = firstFew(dedupeStrings(plan.SearchTerms), 6)
	plan.Measure = firstFew(dedupeStrings(plan.Measure), 8)
	return plan
}

// hasSize is true when something in the item's specifications looks like a
// dimension, which is what a case has to be designed around.
func hasSize(it Item) bool {
	for _, spec := range it.Specs {
		name := strings.ToLower(spec.Name)
		if strings.Contains(name, "size") || strings.Contains(name, "dimension") ||
			strings.Contains(name, "width") || strings.Contains(name, "length") ||
			strings.Contains(name, "height") {
			return true
		}
	}
	return it.Footprint != ""
}

func dedupeStrings(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		key := strings.ToLower(strings.TrimSpace(s))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, s)
	}
	return out
}

func firstFew(in []string, n int) []string {
	if len(in) > n {
		return in[:n]
	}
	return in
}

// --- asking a model ---------------------------------------------------------

const brainstormSystem = `You suggest electronics projects for someone's home workshop.
You are given the parts they have in stock and the tools they own.
Rules:
- Only suggest projects that can be built mostly from the listed parts.
- Name the specific parts from the list that each project uses, spelled exactly as given.
- If a project needs one thing they do not have, list it separately as a missing part; never more than two missing parts.
- Respect the tools: do not tell someone with no 3D printer to print a case, and do not tell someone with no soldering iron to solder.
- Be concrete and specific. No filler, no marketing language.
Reply with JSON only, in this shape:
{"ideas":[{"title":"","summary":"","why":"","effort":"an evening|a weekend|a long project","enclosure":"","parts":[{"name":"","have":true}]}]}`

// modelIdea is the wire shape coming back.
type modelIdea struct {
	Title     string `json:"title"`
	Summary   string `json:"summary"`
	Why       string `json:"why"`
	Effort    string `json:"effort"`
	Enclosure string `json:"enclosure"`
	Parts     []struct {
		Name string `json:"name"`
		Have bool   `json:"have"`
	} `json:"parts"`
}

// SuggestWithModel asks the configured provider for ideas, then checks its
// homework: every part it claims you have is matched back against the actual
// inventory, and anything it invented is marked as missing instead.
func SuggestWithModel(ctx context.Context, p AIProvider, items []Item, caps Capabilities,
	profile Profile, ask string, limit int) ([]Idea, error) {

	prompt := brainstormPrompt(items, caps, profile, ask, limit)
	reply, err := p.Ask(ctx, brainstormSystem, prompt, true)
	if err != nil {
		return nil, err
	}
	var out struct {
		Ideas []modelIdea `json:"ideas"`
	}
	if err := readJSON(reply, &out); err != nil {
		return nil, err
	}
	if len(out.Ideas) == 0 {
		return nil, fmt.Errorf("%s came back with no ideas", p.Name)
	}

	byName := map[string]Item{}
	for _, it := range items {
		byName[strings.ToLower(strings.TrimSpace(it.Name))] = it
	}

	var ideas []Idea
	for _, mi := range out.Ideas {
		if strings.TrimSpace(mi.Title) == "" {
			continue
		}
		idea := Idea{
			Title:     strings.TrimSpace(mi.Title),
			Summary:   strings.TrimSpace(mi.Summary),
			Detail:    strings.TrimSpace(mi.Why),
			Effort:    strings.TrimSpace(mi.Effort),
			Enclosure: strings.TrimSpace(mi.Enclosure),
			Source:    p.Name,
			Status:    "new",
		}
		for _, mp := range mi.Parts {
			name := strings.TrimSpace(mp.Name)
			if name == "" {
				continue
			}
			part := IdeaPart{Name: name, Quantity: 1}
			// The model's own "have" flag is not trusted: the inventory is.
			if it, ok := byName[strings.ToLower(name)]; ok && it.Available() > 0 {
				id := it.ID
				part.ItemID, part.Have, part.Name = &id, true, it.Name
			}
			idea.Parts = append(idea.Parts, part)
		}
		if idea.Enclosure == "" {
			idea.Enclosure = enclosureAdvice(caps, "a box sized around the largest board")
		}
		ideas = append(ideas, idea)
		if limit > 0 && len(ideas) >= limit {
			break
		}
	}
	if len(ideas) == 0 {
		return nil, fmt.Errorf("%s returned nothing usable", p.Name)
	}
	return ideas, nil
}

// brainstormPrompt writes the shelf and the workshop down compactly. It is
// capped because a thousand-part inventory would otherwise be most of the
// context window, and the roles matter more than the long tail.
func brainstormPrompt(items []Item, caps Capabilities, profile Profile, ask string, limit int) string {
	var b strings.Builder
	shelf := ReadShelf(items)

	b.WriteString("Parts in stock, grouped by what they do:\n")
	for _, role := range shelf.Roles {
		list := shelf.ByRole[role]
		var names []string
		for _, it := range firstItems(list, 12) {
			names = append(names, fmt.Sprintf("%s (%d)", it.Name, it.Available()))
		}
		fmt.Fprintf(&b, "- %s: %s\n", role, strings.Join(names, ", "))
	}
	if shelf.Items == 0 {
		b.WriteString("- (nothing in stock yet)\n")
	}

	b.WriteString("\nTools and what they allow:\n")
	can := caps.Can()
	if len(can) == 0 {
		b.WriteString("- no tools recorded; assume only hand assembly of ready-made modules\n")
	}
	for _, c := range can {
		fmt.Fprintf(&b, "- %s\n", c)
	}
	for _, m := range caps.Missing() {
		fmt.Fprintf(&b, "- %s\n", m)
	}

	if profile.Skill != "" {
		fmt.Fprintf(&b, "\nExperience level: %s\n", profile.Skill)
	}
	if profile.Interests != "" {
		fmt.Fprintf(&b, "Interested in: %s\n", profile.Interests)
	}
	if strings.TrimSpace(ask) != "" {
		fmt.Fprintf(&b, "\nWhat they asked for: %s\n", strings.TrimSpace(ask))
	}
	fmt.Fprintf(&b, "\nSuggest %d projects.", limit)
	return b.String()
}

func firstItems(in []Item, n int) []Item {
	if len(in) > n {
		return in[:n]
	}
	return in
}

// --- storage ----------------------------------------------------------------

func (s *Store) ListIdeas(status string) ([]Idea, error) {
	query := `SELECT id, title, summary, detail, status, source, enclosure, effort,
		project_id, created_at FROM ideas`
	var args []any
	switch status {
	case "", "open":
		query += ` WHERE status <> 'dropped'`
	case "all":
	default:
		query += ` WHERE status = ?`
		args = append(args, status)
	}
	query += ` ORDER BY CASE status WHEN 'keeping' THEN 0 WHEN 'new' THEN 1 ELSE 2 END, id DESC`

	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Idea
	byID := map[int64]int{}
	for rows.Next() {
		var i Idea
		var created string
		if err := rows.Scan(&i.ID, &i.Title, &i.Summary, &i.Detail, &i.Status, &i.Source,
			&i.Enclosure, &i.Effort, &i.ProjectID, &created); err != nil {
			return nil, err
		}
		i.CreatedAt, _ = time.Parse(time.RFC3339, created)
		byID[i.ID] = len(out)
		out = append(out, i)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, s.attachIdeaParts(out, byID)
}

// attachIdeaParts fetches the parts for the ideas on the page, in one query
// per chunk rather than one per idea.
func (s *Store) attachIdeaParts(ideas []Idea, byID map[int64]int) error {
	if len(ideas) == 0 {
		return nil
	}
	ids := make([]int64, 0, len(ideas))
	for _, i := range ideas {
		ids = append(ids, i.ID)
	}
	return s.eachInChunks(ids, func(ph string) string {
		return `SELECT id, idea_id, item_id, name, quantity, role, have FROM idea_parts
			WHERE idea_id IN (` + ph + `) ORDER BY id`
	}, func(rows *sql.Rows) error {
		var p IdeaPart
		if err := rows.Scan(&p.ID, &p.IdeaID, &p.ItemID, &p.Name, &p.Quantity, &p.Role, &p.Have); err != nil {
			return err
		}
		if idx, ok := byID[p.IdeaID]; ok {
			ideas[idx].Parts = append(ideas[idx].Parts, p)
		}
		return nil
	})
}

func (s *Store) GetIdea(id int64) (Idea, error) {
	var i Idea
	var created string
	err := s.db.QueryRow(`SELECT id, title, summary, detail, status, source, enclosure,
		effort, project_id, created_at FROM ideas WHERE id = ?`, id).
		Scan(&i.ID, &i.Title, &i.Summary, &i.Detail, &i.Status, &i.Source, &i.Enclosure,
			&i.Effort, &i.ProjectID, &created)
	if err != nil {
		return Idea{}, err
	}
	i.CreatedAt, _ = time.Parse(time.RFC3339, created)
	list := []Idea{i}
	if err := s.attachIdeaParts(list, map[int64]int{i.ID: 0}); err != nil {
		return Idea{}, err
	}
	return list[0], nil
}

// SaveIdeas stores a batch, skipping any whose title is already on the board so
// that generating twice does not fill it with duplicates.
func (s *Store) SaveIdeas(ideas []Idea) (int, error) {
	existing, err := s.ListIdeas("all")
	if err != nil {
		return 0, err
	}
	seen := map[string]bool{}
	for _, i := range existing {
		seen[strings.ToLower(i.Title)] = true
	}
	saved := 0
	for _, idea := range ideas {
		if seen[strings.ToLower(idea.Title)] {
			continue
		}
		if _, err := s.AddIdea(idea); err != nil {
			return saved, err
		}
		seen[strings.ToLower(idea.Title)] = true
		saved++
	}
	return saved, nil
}

func (s *Store) AddIdea(i Idea) (int64, error) {
	if strings.TrimSpace(i.Title) == "" {
		return 0, fmt.Errorf("an idea needs a title")
	}
	if i.Status == "" {
		i.Status = "new"
	}
	res, err := s.db.Exec(`INSERT INTO ideas (title, summary, detail, status, source,
		enclosure, effort, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		i.Title, i.Summary, i.Detail, i.Status, i.Source, i.Enclosure, i.Effort, nowRFC3339())
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	for _, p := range i.Parts {
		if _, err := s.db.Exec(`INSERT INTO idea_parts (idea_id, item_id, name, quantity, role, have)
			VALUES (?,?,?,?,?,?)`, id, p.ItemID, p.Name, max(p.Quantity, 1), p.Role, p.Have); err != nil {
			return id, err
		}
	}
	return id, nil
}

func (s *Store) SetIdeaStatus(id int64, status string) error {
	switch status {
	case "new", "keeping", "dropped", "built":
	default:
		return fmt.Errorf("unknown status %q", status)
	}
	_, err := s.db.Exec(`UPDATE ideas SET status = ? WHERE id = ?`, status, id)
	return err
}

func (s *Store) DeleteIdea(id int64) error {
	_, err := s.db.Exec(`DELETE FROM ideas WHERE id = ?`, id)
	return err
}

// PlanIdea turns an idea into a real project, carrying the parts across so the
// project starts with its bill of materials already filled in.
func (s *Store) PlanIdea(id int64) (int64, error) {
	idea, err := s.GetIdea(id)
	if err != nil {
		return 0, err
	}
	if idea.Planned() {
		return idea.ProjectRef(), nil
	}
	notes := idea.Summary
	if idea.Detail != "" {
		notes = strings.TrimSpace(notes + "\n\n" + idea.Detail)
	}
	if idea.Enclosure != "" {
		notes = strings.TrimSpace(notes + "\n\nEnclosure: " + idea.Enclosure)
	}
	projectID, err := s.CreateProject(idea.Title, notes)
	if err != nil {
		return 0, err
	}
	for _, p := range idea.Parts {
		if err := s.AddProjectPart(projectID, p.ItemID, p.Name, max(p.Quantity, 1), p.Role); err != nil {
			return projectID, err
		}
	}
	_, err = s.db.Exec(`UPDATE ideas SET project_id = ?, status = 'keeping' WHERE id = ?`, projectID, id)
	return projectID, err
}
