// Command upower-exporter exposes UPower power metrics for Prometheus.
//
// On each scrape it enumerates all UPower devices over the system D-Bus and
// exports every numeric/boolean device property as a gauge, plus the UPower
// daemon-level properties (OnBattery, LidIsClosed, ...).
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/godbus/dbus/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	upowerDest       = "org.freedesktop.UPower"
	upowerPath       = dbus.ObjectPath("/org/freedesktop/UPower")
	deviceInterface  = "org.freedesktop.UPower.Device"
	daemonInterface  = "org.freedesktop.UPower"
	enumerateMethod  = "org.freedesktop.UPower.EnumerateDevices"
	getAllProperties = "org.freedesktop.DBus.Properties.GetAll"
)

var camelBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])`)

func snakeCase(s string) string {
	return strings.ToLower(camelBoundary.ReplaceAllString(s, "${1}_${2}"))
}

// toFloat converts numeric and boolean D-Bus property values to float64.
func toFloat(v interface{}) (float64, bool) {
	switch t := v.(type) {
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case float64:
		return t, true
	case int16:
		return float64(t), true
	case uint16:
		return float64(t), true
	case int32:
		return float64(t), true
	case uint32:
		return float64(t), true
	case int64:
		return float64(t), true
	case uint64:
		return float64(t), true
	}
	return 0, false
}

type collector struct {
	conn *dbus.Conn
}

func (c *collector) Describe(chan<- *prometheus.Desc) {
	// Intentionally empty: metrics are dynamic, this is an unchecked collector.
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	up := 1.0
	if err := c.collectDaemon(ch); err != nil {
		log.Printf("collecting daemon properties: %v", err)
		up = 0
	}
	if err := c.collectDevices(ch); err != nil {
		log.Printf("collecting devices: %v", err)
		up = 0
	}
	ch <- prometheus.MustNewConstMetric(
		prometheus.NewDesc("upower_up", "Whether the UPower D-Bus scrape succeeded.", nil, nil),
		prometheus.GaugeValue, up,
	)
}

func (c *collector) collectDaemon(ch chan<- prometheus.Metric) error {
	var props map[string]dbus.Variant
	obj := c.conn.Object(upowerDest, upowerPath)
	if err := obj.Call(getAllProperties, 0, daemonInterface).Store(&props); err != nil {
		return err
	}
	for name, variant := range props {
		val, ok := toFloat(variant.Value())
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			prometheus.NewDesc(
				"upower_"+snakeCase(name),
				fmt.Sprintf("UPower daemon property %s.", name),
				nil, nil,
			),
			prometheus.GaugeValue, val,
		)
	}
	return nil
}

func (c *collector) collectDevices(ch chan<- prometheus.Metric) error {
	var paths []dbus.ObjectPath
	obj := c.conn.Object(upowerDest, upowerPath)
	if err := obj.Call(enumerateMethod, 0).Store(&paths); err != nil {
		return err
	}
	// Include the composite display device as well.
	paths = append(paths, "/org/freedesktop/UPower/devices/DisplayDevice")

	for _, path := range paths {
		var props map[string]dbus.Variant
		dev := c.conn.Object(upowerDest, path)
		if err := dev.Call(getAllProperties, 0, deviceInterface).Store(&props); err != nil {
			log.Printf("device %s: %v", path, err)
			continue
		}

		labels := prometheus.Labels{
			"device":      strings.TrimPrefix(string(path), "/org/freedesktop/UPower/devices/"),
			"native_path": stringProp(props, "NativePath"),
			"model":       stringProp(props, "Model"),
		}
		for name, variant := range props {
			val, ok := toFloat(variant.Value())
			if !ok {
				continue
			}
			ch <- prometheus.MustNewConstMetric(
				prometheus.NewDesc(
					"upower_device_"+snakeCase(name),
					fmt.Sprintf("UPower device property %s.", name),
					nil, labels,
				),
				prometheus.GaugeValue, val,
			)
		}
	}
	return nil
}

func stringProp(props map[string]dbus.Variant, name string) string {
	if v, ok := props[name]; ok {
		if s, ok := v.Value().(string); ok {
			return s
		}
	}
	return ""
}

func main() {
	listenAddr := flag.String("listen-address", ":9459", "Address to listen on for Prometheus scrapes")
	metricsPath := flag.String("metrics-path", "/metrics", "Path under which to expose metrics")
	flag.Parse()

	conn, err := dbus.ConnectSystemBus()
	if err != nil {
		log.Fatalf("connecting to system D-Bus: %v", err)
	}
	defer conn.Close()

	reg := prometheus.NewRegistry()
	reg.MustRegister(&collector{conn: conn})

	http.Handle(*metricsPath, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	http.HandleFunc("/", func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, "<html><body><h1>UPower Exporter</h1><p><a href=%q>Metrics</a></p></body></html>", *metricsPath)
	})

	log.Printf("upower-exporter listening on %s%s", *listenAddr, *metricsPath)
	log.Fatal(http.ListenAndServe(*listenAddr, nil))
}
