package app

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"math"
	"strings"
	"time"
)

// Sensors are the small boards around the workshop that report temperature
// and humidity: one in the filament dry box, one in the printer enclosure, one
// on the wall. Each posts readings here with its own token; the app keeps a
// month of history and says when something is out of range or has gone quiet.

type SensorKind struct {
	Key, Name, Hint, Art string
	MaxHum, MaxTemp      float64 // defaults for a new sensor; 0 is no limit
	MinTemp              float64
}

var SensorKinds = []SensorKind{
	{"drybox", "Dry box", "PETG, nylon and TPU stay printable under about 20% humidity.", "humidity", 20, 0, 0},
	{"enclosure", "Printer enclosure", "PLA softens in the hot end above about 40 °C in a closed box — open a door.", "printer", 0, 40, 0},
	{"room", "Workshop", "Above 60% humidity tools rust and filament on the shelf takes on water.", "sensor", 60, 0, 5},
	{"other", "Something else", "", "sensor", 0, 0, 0},
}

func sensorKind(key string) SensorKind {
	for _, k := range SensorKinds {
		if k.Key == key {
			return k
		}
	}
	return SensorKinds[len(SensorKinds)-1]
}

type Sensor struct {
	ID          int64
	Name        string
	Kind        string
	Location    string
	MaxHumidity float64
	MaxTemp     float64
	MinTemp     float64
	LastAt      time.Time
	LastTemp    *float64
	LastHum     *float64
	CreatedAt   time.Time
}

func (s Sensor) KindInfo() SensorKind { return sensorKind(s.Kind) }
func (s Sensor) HasReading() bool     { return !s.LastAt.IsZero() }

// Quiet means it was reporting and has stopped: flat battery, Wi-Fi gone,
// unplugged. Thirty minutes is long enough not to fire on a reboot.
func (s Sensor) Quiet() bool { return s.HasReading() && time.Since(s.LastAt) > 30*time.Minute }

func (s Sensor) TempLabel() string {
	if s.LastTemp == nil {
		return ""
	}
	return fmt.Sprintf("%.1f °C", *s.LastTemp)
}

func (s Sensor) HumLabel() string {
	if s.LastHum == nil {
		return ""
	}
	return fmt.Sprintf("%.0f%%", *s.LastHum)
}

// Alerts is what is wrong right now, in words. A quiet sensor's last reading
// is not trusted to raise anything else.
func (s Sensor) Alerts() []string {
	if !s.HasReading() {
		return nil
	}
	if s.Quiet() {
		return []string{fmt.Sprintf("%s has not reported since %s", s.Name, s.LastAt.Local().Format("15:04, 2 Jan"))}
	}
	var out []string
	if s.LastHum != nil && s.MaxHumidity > 0 && *s.LastHum > s.MaxHumidity {
		msg := fmt.Sprintf("%s is at %.0f%% humidity (limit %.0f%%)", s.Name, *s.LastHum, s.MaxHumidity)
		if s.Kind == "drybox" {
			msg += " — time to change the desiccant"
		}
		out = append(out, msg)
	}
	if s.LastTemp != nil && s.MaxTemp > 0 && *s.LastTemp > s.MaxTemp {
		out = append(out, fmt.Sprintf("%s is at %.1f °C (limit %.0f °C)", s.Name, *s.LastTemp, s.MaxTemp))
	}
	if s.LastTemp != nil && s.MinTemp != 0 && *s.LastTemp < s.MinTemp {
		out = append(out, fmt.Sprintf("%s is down to %.1f °C (limit %.0f °C)", s.Name, *s.LastTemp, s.MinTemp))
	}
	return out
}

func (s Sensor) HumHigh() bool {
	return s.LastHum != nil && s.MaxHumidity > 0 && *s.LastHum > s.MaxHumidity
}

func (s Sensor) TempOut() bool {
	return s.LastTemp != nil && ((s.MaxTemp > 0 && *s.LastTemp > s.MaxTemp) || (s.MinTemp != 0 && *s.LastTemp < s.MinTemp))
}

func (s Sensor) Alarm() bool { return len(s.Alerts()) > 0 }

// --- tokens ----------------------------------------------------------------------

// newSensorToken makes a token and the hash that is stored. The token itself is
// shown once and never kept.
func newSensorToken() (token, hash string) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic(err) // the system's randomness failing is not something to carry on from
	}
	token = "bos_" + hex.EncodeToString(b)
	return token, hashToken(token)
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(token)))
	return hex.EncodeToString(sum[:])
}

// --- readings ----------------------------------------------------------------------

type Reading struct {
	At   time.Time
	Temp *float64
	Hum  *float64
}

// validReading refuses values no workshop sensor produces, which is what a
// disconnected DHT22 reports (-999, 0xFFFF, NaN).
func validReading(temp, hum *float64) error {
	if temp == nil && hum == nil {
		return fmt.Errorf("send temperature, humidity, or both")
	}
	if temp != nil && (math.IsNaN(*temp) || *temp < -50 || *temp > 150) {
		return fmt.Errorf("temperature %.1f is not a reading — is the sensor connected?", *temp)
	}
	if hum != nil && (math.IsNaN(*hum) || *hum < 0 || *hum > 100) {
		return fmt.Errorf("humidity %.1f is not a reading — is the sensor connected?", *hum)
	}
	return nil
}

// --- storage ---------------------------------------------------------------------

const sensorCols = `id, name, kind, location, max_humidity, max_temp, min_temp, last_at, last_temp, last_hum, created_at`

func scanSensor(row rowScanner) (Sensor, error) {
	var s Sensor
	var last, created string
	var t, h sql.NullFloat64
	err := row.Scan(&s.ID, &s.Name, &s.Kind, &s.Location, &s.MaxHumidity, &s.MaxTemp, &s.MinTemp,
		&last, &t, &h, &created)
	if t.Valid {
		v := t.Float64
		s.LastTemp = &v
	}
	if h.Valid {
		v := h.Float64
		s.LastHum = &v
	}
	s.LastAt, _ = time.Parse(time.RFC3339, last)
	s.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return s, err
}

func (st *Store) AddSensor(s Sensor) (int64, string, error) {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return 0, "", fmt.Errorf("give it a name, like “dry box 1”")
	}
	token, hash := newSensorToken()
	res, err := st.db.Exec(`INSERT INTO sensors (name, kind, location, token_hash, max_humidity,
		max_temp, min_temp, created_at) VALUES (?,?,?,?,?,?,?,?)`,
		s.Name, sensorKind(s.Kind).Key, strings.TrimSpace(s.Location), hash, s.MaxHumidity, s.MaxTemp,
		s.MinTemp, nowRFC3339())
	if err != nil {
		return 0, "", err
	}
	id, err := res.LastInsertId()
	return id, token, err
}

func (st *Store) UpdateSensor(s Sensor) error {
	s.Name = strings.TrimSpace(s.Name)
	if s.Name == "" {
		return fmt.Errorf("give it a name")
	}
	_, err := st.db.Exec(`UPDATE sensors SET name = ?, kind = ?, location = ?, max_humidity = ?,
		max_temp = ?, min_temp = ? WHERE id = ?`, s.Name, sensorKind(s.Kind).Key,
		strings.TrimSpace(s.Location), s.MaxHumidity, s.MaxTemp, s.MinTemp, s.ID)
	return err
}

// NewSensorToken replaces a sensor's token, for when the old one leaked or the
// board was lost. The old one stops working at once.
func (st *Store) NewSensorToken(id int64) (string, error) {
	token, hash := newSensorToken()
	res, err := st.db.Exec(`UPDATE sensors SET token_hash = ? WHERE id = ?`, hash, id)
	if err != nil {
		return "", err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return "", sql.ErrNoRows
	}
	return token, nil
}

func (st *Store) DeleteSensor(id int64) error {
	_, err := st.db.Exec(`DELETE FROM sensors WHERE id = ?`, id)
	return err
}

func (st *Store) GetSensor(id int64) (Sensor, error) {
	return scanSensor(st.db.QueryRow(`SELECT `+sensorCols+` FROM sensors WHERE id = ?`, id))
}

func (st *Store) SensorByToken(token string) (Sensor, error) {
	return scanSensor(st.db.QueryRow(`SELECT `+sensorCols+` FROM sensors WHERE token_hash = ?`, hashToken(token)))
}

func (st *Store) ListSensors() ([]Sensor, error) {
	rows, err := st.db.Query(`SELECT ` + sensorCols + ` FROM sensors ORDER BY name COLLATE NOCASE`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sensor
	for rows.Next() {
		s, err := scanSensor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// sensorKeep is how much history is kept. A month shows a desiccant wearing
// out; more is a time-series database's job.
const sensorKeep = 31 * 24 * time.Hour

// AddReading records one reading and moves the sensor's latest values. A value
// left out keeps the last one, so a board that only sends humidity does not
// blank the temperature.
func (st *Store) AddReading(id int64, temp, hum *float64, at time.Time) error {
	stamp := at.UTC().Format(time.RFC3339)
	tx, err := st.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO sensor_readings (sensor_id, at, temp, hum) VALUES (?,?,?,?)`,
		id, stamp, temp, hum); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE sensors SET last_at = ?, last_temp = COALESCE(?, last_temp),
		last_hum = COALESCE(?, last_hum) WHERE id = ?`, stamp, temp, hum, id); err != nil {
		return err
	}
	cutoff := at.Add(-sensorKeep).UTC().Format(time.RFC3339)
	if _, err := tx.Exec(`DELETE FROM sensor_readings WHERE sensor_id = ? AND at < ?`, id, cutoff); err != nil {
		return err
	}
	return tx.Commit()
}

// Readings returns a sensor's history since a time, oldest first, averaged
// down to at most n points so a month draws as fast as an hour.
func (st *Store) Readings(id int64, since time.Time, n int) ([]Reading, error) {
	rows, err := st.db.Query(`SELECT at, temp, hum FROM sensor_readings
		WHERE sensor_id = ? AND at >= ? ORDER BY at`, id, since.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var all []Reading
	for rows.Next() {
		var at string
		var t, h sql.NullFloat64
		if err := rows.Scan(&at, &t, &h); err != nil {
			return nil, err
		}
		r := Reading{}
		r.At, _ = time.Parse(time.RFC3339, at)
		if t.Valid {
			v := t.Float64
			r.Temp = &v
		}
		if h.Valid {
			v := h.Float64
			r.Hum = &v
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return downsample(all, n), nil
}

// downsample averages consecutive readings into n buckets.
func downsample(in []Reading, n int) []Reading {
	if n <= 0 || len(in) <= n {
		return in
	}
	out := make([]Reading, 0, n)
	per := float64(len(in)) / float64(n)
	for b := 0; b < n; b++ {
		lo, hi := int(float64(b)*per), int(float64(b+1)*per)
		if hi > len(in) {
			hi = len(in)
		}
		var ts, hs float64
		var tn, hn int
		for _, r := range in[lo:hi] {
			if r.Temp != nil {
				ts += *r.Temp
				tn++
			}
			if r.Hum != nil {
				hs += *r.Hum
				hn++
			}
		}
		r := Reading{At: in[hi-1].At}
		if tn > 0 {
			v := ts / float64(tn)
			r.Temp = &v
		}
		if hn > 0 {
			v := hs / float64(hn)
			r.Hum = &v
		}
		out = append(out, r)
	}
	return out
}
