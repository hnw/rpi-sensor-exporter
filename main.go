// Package main implements a prometheus exporter for rpi sensors.
package main

import (
	"log"
	"math"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"gobot.io/x/gobot/drivers/i2c"
	"gobot.io/x/gobot/platforms/raspi"
)

// センサーの更新間隔
const sensorUpdateInterval = 5 * time.Second

// --- Prometheus Metrics Definitions ---
var (
	tempGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sensor_temperature_celsius",
		Help: "Temperature in Celsius",
	}, []string{"device", "location"})

	humGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sensor_humidity_percent",
		Help: "Relative Humidity in Percent",
	}, []string{"device", "location"})

	absHumGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sensor_absolute_humidity_g_m3",
		Help: "Absolute Humidity in g/m^3 (Calculated via Bolton's equation)",
	}, []string{"device", "location"})

	pressGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sensor_pressure_hpa",
		Help: "Pressure in hPa",
	}, []string{"device", "location"})

	luxGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sensor_illuminance_lux",
		Help: "Illuminance in Lux (Calculated)",
	}, []string{"device", "location"})

	rawIllumGauge = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "sensor_light_raw",
		Help: "Raw light sensor values",
	}, []string{"device", "location", "type"}) // type: broadband, infrared
)

func init() {
	prometheus.MustRegister(
		tempGauge, humGauge, absHumGauge, pressGauge,
		luxGauge, rawIllumGauge,
	)
}

// Calculate absolute humidity(g/m^3) from temperature(C) and relative humidity(%)
// This is based on Bolton's equation[1].
// [1] Bolton, D., The computation of equivalent potential temperature, Monthly Weather Review, 108, 1046-1053, 1980.
func calcAbsoluteHumidity(t float64, rh float64) (ah float64) {
	ah = 6.112 * math.Exp(17.67*t/(t+243.5)) * rh * 2.1674 / (273.15 + t)
	return
}

func main() {
	// 1. Initialize Adaptor (Raspberry Pi)
	r := raspi.NewAdaptor()

	// 3. Initialize I2C Drivers
	// BME280 (Default Address 0x77)
	bme := i2c.NewBME280Driver(r)

	// SHT2x (Default Address 0x40)
	sht := i2c.NewSHT2xDriver(r)

	// TSL2561 (Address 0x29, Gain 16X)
	tsl := i2c.NewTSL2561Driver(r, i2c.WithTSL2561Gain16X, i2c.WithAddress(0x29))

	// 4. Connect to Hardware
	if err := r.Connect(); err != nil {
		log.Fatalf("Raspberry Pi connect failed: %v", err)
	}

	log.Println("Initializing sensors...")

	// Start Sensors (Log errors but continue)
	if err := bme.Start(); err != nil {
		log.Printf("⚠️ BME280 init failed: %v", err)
		bme = nil
	}
	if err := sht.Start(); err != nil {
		log.Printf("⚠️ SHT2x init failed: %v", err)
		sht = nil
	}
	if err := tsl.Start(); err != nil {
		log.Printf("⚠️ TSL2561 init failed: %v", err)
		tsl = nil
	}

	// 5. Background Update Loop (Goroutine)
	go func() {
		// Update immediately on start
		updateSensors(bme, sht, tsl)

		ticker := time.NewTicker(sensorUpdateInterval)
		defer ticker.Stop()

		for range ticker.C {
			updateSensors(bme, sht, tsl)
		}
	}()

	// 6. HTTP Handler
	http.Handle("/metrics", promhttp.Handler())
	port := getEnv("PORT", "9101")
	log.Println("rpi-sensor-exporter listening on :" + port)

	server := &http.Server{
		Addr:              ":" + port,
		ReadHeaderTimeout: 3 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func updateSensors(
	bme *i2c.BME280Driver, sht *i2c.SHT2xDriver, tsl *i2c.TSL2561Driver,
) {
	// --- BME280 ---
	if bme != nil {
		t, errT := bme.Temperature()
		p, errP := bme.Pressure()
		h, errH := bme.Humidity()

		if errT != nil || errP != nil || errH != nil {
			log.Printf("BME280 read error: T=%v, P=%v, H=%v", errT, errP, errH)
		} else {
			tempGauge.WithLabelValues("bme280", "indoor").Set(float64(t))
			pressGauge.WithLabelValues("bme280", "indoor").Set(float64(p) / 100.0) // Pa -> hPa
			humGauge.WithLabelValues("bme280", "indoor").Set(float64(h))

			// Calc Absolute Humidity
			ah := calcAbsoluteHumidity(float64(t), float64(h))
			absHumGauge.WithLabelValues("bme280", "indoor").Set(ah)
		}
	}

	// --- SHT2x ---
	if sht != nil {
		t, errT := sht.Temperature()
		h, errH := sht.Humidity()

		if errT != nil || errH != nil {
			log.Printf("SHT2x read error: T=%v, H=%v", errT, errH)
		} else {
			tempGauge.WithLabelValues("sht2x", "indoor").Set(float64(t))
			humGauge.WithLabelValues("sht2x", "indoor").Set(float64(h))

			// Calc Absolute Humidity
			ah := calcAbsoluteHumidity(float64(t), float64(h))
			absHumGauge.WithLabelValues("sht2x", "indoor").Set(ah)
		}
	}

	// --- TSL2561 ---
	if tsl != nil {
		bb, ir, err := tsl.GetLuminocity()
		if err != nil {
			log.Printf("TSL2561 read error: %v", err)
		} else {
			// Calculate Lux using Gobot's internal helper
			lux := tsl.CalculateLux(bb, ir)

			luxGauge.WithLabelValues("tsl2561", "indoor").Set(float64(lux))
			rawIllumGauge.WithLabelValues("tsl2561", "indoor", "broadband").Set(float64(bb))
			rawIllumGauge.WithLabelValues("tsl2561", "indoor", "infrared").Set(float64(ir))
		}
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
