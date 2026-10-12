package app

import "strings"

// tagIcons maps a substring of a tag name to the emoji shown beside it. The
// first match in this order wins, so more specific words are listed first --
// "esp32" must beat the bare "32", and "usb-c" must beat "usb".
//
// This exists so tags look like something without anyone having to configure
// icons. Anything unmatched still gets an icon from iconFallback.
var tagIcons = []struct{ match, icon string }{
	// Boards and silicon
	{"esp32", "📡"}, {"esp8266", "📡"}, {"rp2040", "🫐"}, {"raspberry", "🍓"},
	{"arduino", "🔷"}, {"stm32", "🔶"}, {"attiny", "🐜"}, {"avr", "🔶"},
	{"samd", "🔶"}, {"riscv", "🧮"}, {"risc-v", "🧮"}, {"arm", "🧮"},
	{"fpga", "🧩"}, {"mcu", "🧠"}, {"sbc", "🖥"}, {"feather", "🪶"},
	{"pico", "🫐"}, {"teensy", "🧠"}, {"m5stick", "📟"}, {"m5", "📟"},

	// Connectivity
	{"wifi", "📶"}, {"bluetooth", "🔵"}, {"ble", "🔵"}, {"zigbee", "🐝"},
	{"lora", "🛰"}, {"ethernet", "🔌"}, {"gigabit", "🔌"}, {"rf", "📻"},
	{"nfc", "💳"}, {"gps", "🛰"}, {"antenna", "📡"},

	// Buses
	{"i2c", "🔗"}, {"spi", "🔗"}, {"uart", "🔗"}, {"serial", "🔗"},
	{"1-wire", "🧵"}, {"onewire", "🧵"}, {"can", "🚌"}, {"usb-c", "🔌"},
	{"usb", "🔌"}, {"qwiic", "🔗"}, {"stemma", "🔗"},

	// Sensing
	{"temperature", "🌡"}, {"temp", "🌡"}, {"humidity", "💧"}, {"pressure", "🎈"},
	{"imu", "🧭"}, {"accel", "🧭"}, {"gyro", "🧭"}, {"compass", "🧭"},
	{"light", "💡"}, {"lux", "💡"}, {"motion", "🏃"}, {"pir", "🏃"},
	{"distance", "📏"}, {"ultrasonic", "📏"}, {"current", "⚡"}, {"sensor", "🎛"},
	{"co2", "🫁"}, {"gas", "🫁"}, {"sound", "🎤"}, {"mic", "🎤"},

	// Output
	{"display", "🖵"}, {"oled", "🖵"}, {"lcd", "🖵"}, {"tft", "🖵"},
	{"eink", "📄"}, {"e-ink", "📄"}, {"epaper", "📄"}, {"led", "💡"},
	{"neopixel", "🌈"}, {"rgb", "🌈"}, {"buzzer", "🔔"}, {"speaker", "🔊"},
	{"relay", "🔀"}, {"servo", "⚙"}, {"stepper", "⚙"}, {"motor", "⚙"},

	// Passives and packaging
	{"resistor", "🪛"}, {"capacitor", "🔋"}, {"decoupling", "🔋"},
	{"inductor", "🌀"}, {"diode", "🔺"}, {"transistor", "🔻"}, {"mosfet", "🔻"},
	{"crystal", "💎"}, {"passive", "🪛"}, {"smd", "▫"}, {"tht", "▪"},
	{"0402", "▫"}, {"0603", "▫"}, {"0805", "▫"}, {"1206", "▫"},
	{"connector", "🔌"}, {"header", "📌"}, {"jumper", "🧷"}, {"cable", "🧵"},
	{"screw", "🔩"}, {"standoff", "🔩"}, {"enclosure", "📦"},

	// Power
	{"battery", "🔋"}, {"lipo", "🔋"}, {"18650", "🔋"}, {"charger", "🔌"},
	{"buck", "⚡"}, {"boost", "⚡"}, {"regulator", "⚡"}, {"psu", "⚡"},
	{"3v3", "⚡"}, {"5v", "⚡"}, {"12v", "⚡"}, {"solar", "☀"},

	// Workflow
	{"tool", "🛠"}, {"solder", "🛠"}, {"3d", "🖨"}, {"printer", "🖨"},
	{"filament", "🧶"}, {"broken", "💥"}, {"spare", "📦"}, {"todo", "📝"},
	{"kit", "🎁"}, {"camera", "📷"}, {"audio", "🎵"}, {"sd", "💾"},
	{"storage", "💾"}, {"flash", "💾"}, {"clock", "🕐"}, {"rtc", "🕐"},
}

// iconFallback is picked by hashing the tag name, so an unrecognised tag still
// gets a stable icon rather than an empty gap.
var iconFallback = []string{"🔧", "🧰", "📦", "🔩", "🧲", "🪫", "🧊", "🪛"}

// IconForTag chooses an emoji for a tag name. It is only a default: the icon is
// stored per tag and can be changed afterwards.
func IconForTag(name string) string {
	lower := strings.ToLower(strings.TrimSpace(name))
	if lower == "" {
		return ""
	}
	for _, t := range tagIcons {
		if strings.Contains(lower, t.match) {
			return t.icon
		}
	}
	var sum int
	for _, r := range lower {
		sum = sum*31 + int(r)
		if sum < 0 {
			sum = -sum
		}
	}
	return iconFallback[sum%len(iconFallback)]
}
