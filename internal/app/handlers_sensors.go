package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type sensorsData struct {
	Sensors []sensorCard
	Kinds   []SensorKind
}

type sensorCard struct {
	Sensor
	Spark template.HTML
}

type sensorData struct {
	Sensor   Sensor
	Kinds    []SensorKind
	Token    string // only right after it was made; never stored
	Endpoint string
	Hum      template.HTML
	Temp     template.HTML
	Count    int
}

func (a *App) handleSensors(w http.ResponseWriter, r *http.Request) {
	sensors, err := a.store.ListSensors()
	if err != nil {
		a.fail(w, err, http.StatusInternalServerError)
		return
	}
	cards := make([]sensorCard, 0, len(sensors))
	for _, s := range sensors {
		readings, _ := a.store.Readings(s.ID, time.Now().Add(-24*time.Hour), 96)
		which, limit := "hum", s.MaxHumidity
		if s.Kind == "enclosure" || !anyHum(readings) {
			which, limit = "temp", s.MaxTemp
		}
		cards = append(cards, sensorCard{s, template.HTML(SensorChart(readings, which, limit, 240, 56, false))})
	}
	a.render(w, r, "sensors.html", "Sensors", sensorsData{Sensors: cards, Kinds: SensorKinds})
}

func anyHum(rs []Reading) bool {
	for _, r := range rs {
		if r.Hum != nil {
			return true
		}
	}
	return false
}

func sensorFromForm(r *http.Request) Sensor {
	num := func(k string) float64 {
		v, _ := strconv.ParseFloat(strings.TrimSpace(r.FormValue(k)), 64)
		return v
	}
	return Sensor{
		Name: r.FormValue("name"), Kind: r.FormValue("kind"), Location: r.FormValue("location"),
		MaxHumidity: num("max_humidity"), MaxTemp: num("max_temp"), MinTemp: num("min_temp"),
	}
}

func (a *App) handleAddSensor(w http.ResponseWriter, r *http.Request) {
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	s := sensorFromForm(r)
	// A new sensor starts with its kind's sensible limits unless told otherwise.
	k := sensorKind(s.Kind)
	if r.FormValue("max_humidity") == "" {
		s.MaxHumidity = k.MaxHum
	}
	if r.FormValue("max_temp") == "" {
		s.MaxTemp = k.MaxTemp
	}
	if r.FormValue("min_temp") == "" {
		s.MinTemp = k.MinTemp
	}
	id, token, err := a.store.AddSensor(s)
	if err != nil {
		redirect(w, r, "/sensors", "", err.Error())
		return
	}
	a.store.Record(a.actor(r), "added a sensor", "sensor", id, s.Name)
	// The token is shown on this response and nowhere else: it is not put in
	// a redirect URL, where it would sit in the browser's history.
	a.showSensor(w, r, id, token, "Added "+strings.TrimSpace(s.Name)+" — copy its token now, it is only shown once")
}

func (a *App) handleSensor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	a.showSensor(w, r, id, "", "")
}

func (a *App) showSensor(w http.ResponseWriter, r *http.Request, id int64, token, flash string) {
	s, err := a.store.GetSensor(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	week, _ := a.store.Readings(id, time.Now().Add(-7*24*time.Hour), 240)
	var count int
	a.store.db.QueryRow(`SELECT COUNT(*) FROM sensor_readings WHERE sensor_id = ?`, id).Scan(&count)
	if flash != "" {
		q := r.URL.Query()
		q.Set("flash", flash)
		r.URL.RawQuery = q.Encode()
	}
	a.render(w, r, "sensor.html", s.Name, sensorData{
		Sensor: s, Kinds: SensorKinds, Token: token,
		Endpoint: a.BaseURL(r) + "/api/sensors/reading",
		Hum:      template.HTML(SensorChart(week, "hum", s.MaxHumidity, 640, 150, true)),
		Temp:     template.HTML(SensorChart(week, "temp", s.MaxTemp, 640, 150, true)),
		Count:    count,
	})
}

func (a *App) handleUpdateSensor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := parseForm(r); err != nil {
		a.fail(w, err, http.StatusBadRequest)
		return
	}
	s := sensorFromForm(r)
	s.ID = id
	dest := fmt.Sprintf("/sensors/%d", id)
	if err := a.store.UpdateSensor(s); err != nil {
		redirect(w, r, dest, "", err.Error())
		return
	}
	redirect(w, r, dest, "Saved", "")
}

func (a *App) handleSensorToken(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	token, err := a.store.NewSensorToken(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.store.Record(a.actor(r), "replaced a sensor token", "sensor", id, "")
	a.showSensor(w, r, id, token, "New token made — the old one no longer works. Put this one on the board.")
}

func (a *App) handleDeleteSensor(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	s, err := a.store.GetSensor(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	a.store.DeleteSensor(id)
	a.store.Record(a.actor(r), "deleted a sensor", "sensor", id, s.Name)
	redirect(w, r, "/sensors", "Deleted "+s.Name, "")
}

// --- the endpoint the boards post to -------------------------------------------------

// handleSensorReading takes one reading from a board. It sits outside sign-in
// — a board cannot log in — and is authorised by the sensor's own token
// instead, which only lets it add readings to itself.
//
// It takes JSON ({"temperature": 21.4, "humidity": 18}) or a plain form post,
// and the short names ESPHome and Arduino sketches tend to use.
func (a *App) handleSensorReading(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		replyJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "a reading is a few numbers, not that much"})
		return
	}
	fields := map[string]any{}
	if t := bytes.TrimSpace(body); len(t) > 0 && t[0] == '{' {
		if err := json.Unmarshal(t, &fields); err != nil {
			replyJSON(w, http.StatusBadRequest, map[string]string{"error": "that JSON did not parse"})
			return
		}
	} else {
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := r.ParseForm(); err == nil {
			for k := range r.PostForm {
				fields[k] = r.PostForm.Get(k)
			}
		}
	}

	token := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if token == "" || token == r.Header.Get("Authorization") {
		token = r.Header.Get("X-Sensor-Token")
	}
	if token == "" {
		if t, ok := fields["token"].(string); ok {
			token = t
		}
	}
	if token == "" {
		replyJSON(w, http.StatusUnauthorized, map[string]string{"error": "send the sensor's token as Authorization: Bearer …"})
		return
	}
	s, err := a.store.SensorByToken(token)
	if err != nil {
		replyJSON(w, http.StatusUnauthorized, map[string]string{"error": "no sensor has that token"})
		return
	}
	// A board stuck in a loop should not fill the disk. One every ten seconds
	// is far more than a temperature needs.
	if s.HasReading() && time.Since(s.LastAt) < 10*time.Second {
		w.Header().Set("Retry-After", "10")
		replyJSON(w, http.StatusTooManyRequests, map[string]string{"error": "one reading every ten seconds is plenty"})
		return
	}

	temp := pickNumber(fields, "temperature", "temp", "t", "temp_c", "temperature_c")
	hum := pickNumber(fields, "humidity", "hum", "h", "rh", "relative_humidity")
	if err := validReading(temp, hum); err != nil {
		replyJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := a.store.AddReading(s.ID, temp, hum, time.Now()); err != nil {
		replyJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not store it"})
		return
	}
	s, _ = a.store.GetSensor(s.ID)
	out := map[string]any{"ok": true, "sensor": s.Name}
	if alerts := s.Alerts(); len(alerts) > 0 {
		out["alert"] = strings.Join(alerts, "; ")
	}
	replyJSON(w, http.StatusOK, out)
}

// pickNumber finds the first of the names present, as a number or a string of
// one. ESPHome sends floats; a form post sends text.
func pickNumber(fields map[string]any, names ...string) *float64 {
	for _, n := range names {
		for k, v := range fields {
			if !strings.EqualFold(k, n) {
				continue
			}
			var f float64
			switch x := v.(type) {
			case float64:
				f = x
			case string:
				var err error
				if f, err = strconv.ParseFloat(strings.TrimSpace(x), 64); err != nil {
					continue
				}
			default:
				continue
			}
			return &f
		}
	}
	return nil
}

// --- drawing ---------------------------------------------------------------------

// SensorChart draws one series as an SVG line with its limit dashed across it.
// Every value in it is a number formatted here, so it is safe to inline.
func SensorChart(rs []Reading, which string, limit float64, w, h float64, axes bool) string {
	type pt struct {
		t time.Time
		v float64
	}
	var pts []pt
	for _, r := range rs {
		p := r.Temp
		if which == "hum" {
			p = r.Hum
		}
		if p != nil {
			pts = append(pts, pt{r.At, *p})
		}
	}
	if len(pts) < 2 {
		return `<p class="muted small chart-empty">Not enough readings to draw yet.</p>`
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, p := range pts {
		lo, hi = math.Min(lo, p.v), math.Max(hi, p.v)
	}
	if limit > 0 {
		lo, hi = math.Min(lo, limit), math.Max(hi, limit)
	}
	pad := math.Max((hi-lo)*0.12, 1)
	lo, hi = lo-pad, hi+pad
	if which == "hum" {
		lo, hi = math.Max(0, lo), math.Min(100, hi)
	}
	left, bottom := 0.0, 0.0
	if axes {
		left, bottom = 38, 18
	}
	t0, t1 := pts[0].t, pts[len(pts)-1].t
	span := t1.Sub(t0).Seconds()
	if span <= 0 {
		span = 1
	}
	x := func(t time.Time) float64 { return left + (w-left-2)*t.Sub(t0).Seconds()/span }
	y := func(v float64) float64 { return 2 + (h-bottom-4)*(1-(v-lo)/(hi-lo)) }

	var line strings.Builder
	for i, p := range pts {
		if i > 0 {
			line.WriteByte(' ')
		}
		fmt.Fprintf(&line, "%.1f,%.1f", x(p.t), y(p.v))
	}
	unit := "°C"
	if which == "hum" {
		unit = "%"
	}
	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="chart chart-%s" viewBox="0 0 %.0f %.0f" preserveAspectRatio="none" role="img" aria-label="%s over time">`,
		which, w, h, map[string]string{"hum": "Humidity", "temp": "Temperature"}[which])
	if axes {
		for _, v := range []float64{lo, (lo + hi) / 2, hi} {
			fmt.Fprintf(&b, `<line class="grid" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/><text x="%.0f" y="%.1f" class="ax">%.0f%s</text>`,
				left, w, y(v), y(v), left-4, y(v)+4, v, unit)
		}
		fmt.Fprintf(&b, `<text x="%.0f" y="%.0f" class="ax ax-l">%s</text><text x="%.0f" y="%.0f" class="ax">%s</text>`,
			left, h-3, t0.Local().Format("Mon 15:04"), w-2, h-3, t1.Local().Format("Mon 15:04"))
	}
	if limit > 0 {
		fmt.Fprintf(&b, `<line class="limit" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/>`, left, w, y(limit), y(limit))
	}
	fmt.Fprintf(&b, `<polyline class="area" points="%.1f,%.1f %s %.1f,%.1f"/>`, x(t0), h-bottom, line.String(), x(t1), h-bottom)
	fmt.Fprintf(&b, `<polyline class="line" points="%s"/></svg>`, line.String())
	return b.String()
}
