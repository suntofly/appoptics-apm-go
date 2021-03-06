// Copyright (C) 2017 Librato, Inc. All rights reserved.

package reporter

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/appoptics/appoptics-apm-go/v1/ao/internal/hdrhist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/mgo.v2/bson"
)

func bsonToMap(bbuf *bsonBuffer) map[string]interface{} {
	m := make(map[string]interface{})
	bson.Unmarshal(bbuf.GetBuf(), m)
	return m
}

func round(val float64, roundOn float64, places int) (newVal float64) {
	var round float64
	pow := math.Pow(10, float64(places))
	digit := pow * val
	_, div := math.Modf(digit)
	if div >= roundOn {
		round = math.Ceil(digit)
	} else {
		round = math.Floor(digit)
	}
	newVal = round / pow
	return
}

func TestDistro(t *testing.T) {
	distro := strings.ToLower(getDistro())

	assert.NotEmpty(t, distro)

	if runtime.GOOS == "linux" {
		assert.NotContains(t, distro, "unknown")
	} else {
		assert.Contains(t, distro, "unknown")
	}
}

func TestAppendIPAddresses(t *testing.T) {
	bbuf := NewBsonBuffer()
	appendIPAddresses(bbuf)
	bsonBufferFinish(bbuf)
	m := bsonToMap(bbuf)

	ifaces, _ := net.Interfaces()
	var addresses []string

	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() && isPhysicalInterface(iface.Name) {
				addresses = append(addresses, ipnet.IP.String())
			}
		}
	}

	if m["IPAddresses"] != nil {
		bsonIPs := m["IPAddresses"].([]interface{})
		assert.Equal(t, len(bsonIPs), len(addresses))

		for i := 0; i < len(bsonIPs); i++ {
			assert.Equal(t, bsonIPs[i], addresses[i])
		}
	} else {
		assert.Equal(t, 0, len(addresses))
	}
}

func TestAppendMACAddresses(t *testing.T) {
	bbuf := NewBsonBuffer()
	appendMACAddresses(bbuf)
	bsonBufferFinish(bbuf)
	m := bsonToMap(bbuf)

	ifaces, _ := net.Interfaces()
	var macs []string
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		if !isPhysicalInterface(iface.Name) {
			continue
		}
		if mac := iface.HardwareAddr.String(); mac != "" {
			macs = append(macs, iface.HardwareAddr.String())
		}
	}

	if m["MACAddresses"] != nil {
		bsonMACs := m["MACAddresses"].([]interface{})
		assert.Equal(t, len(bsonMACs), len(macs))

		for i := 0; i < len(bsonMACs); i++ {
			assert.Equal(t, bsonMACs[i], macs[i])
		}
	} else {
		assert.Equal(t, 0, len(macs))
	}
}

func TestIsEC2Instance(t *testing.T) {
	file1 := "/tmp/TestIsEC2Instance1"
	file2 := "/tmp/TestIsEC2Instance2"

	// test with one file that contains garbage
	ioutil.WriteFile(file1, []byte("garbage"), 0644)
	assert.False(t, isEC2Instance([]string{file1}))
	cachedIsEC2Instance = nil
	os.Remove(file1)

	// test with one file that contains valid EC2 UUID (lowercase)
	ioutil.WriteFile(file1, []byte("ec2e1916-9099-7caf-fd21-012345abcdef"), 0644)
	assert.True(t, isEC2Instance([]string{file1}))
	cachedIsEC2Instance = nil
	os.Remove(file1)

	// test with one file that contains valid EC2 UUID (uppercase)
	ioutil.WriteFile(file1, []byte("EC2E1916-9099-7CAF-FD21-01234ABCDEF"), 0644)
	assert.True(t, isEC2Instance([]string{file1}))
	cachedIsEC2Instance = nil
	os.Remove(file1)

	// test with one file that contains invalid EC2 UUID
	ioutil.WriteFile(file1, []byte("EC3E1916-9099-7CAF-FD21-01234ABCDEF"), 0644)
	assert.False(t, isEC2Instance([]string{file1}))
	cachedIsEC2Instance = nil
	os.Remove(file1)

	// test with one file that contains valid EC2 UUID (lowercase, little endian format)
	ioutil.WriteFile(file1, []byte("45e12aec-dcd1-b213-94ed-01234abcdef"), 0644)
	assert.True(t, isEC2Instance([]string{file1}))
	cachedIsEC2Instance = nil
	os.Remove(file1)

	// test with one file that contains valid EC2 UUID (uppercase, little endian format)
	ioutil.WriteFile(file1, []byte("45E12AEC-DCD1-B213-94ED-01234ABCDEF"), 0644)
	assert.True(t, isEC2Instance([]string{file1}))
	cachedIsEC2Instance = nil
	os.Remove(file1)

	// test with two files with the second one containing valid EC2 UUID
	ioutil.WriteFile(file1, []byte("garbage"), 0644)
	ioutil.WriteFile(file2, []byte("ec2e1916-9099-7caf-fd21-012345abcdef"), 0644)
	assert.True(t, isEC2Instance([]string{file1, file2}))
	cachedIsEC2Instance = nil
	os.Remove(file1)
	os.Remove(file2)

	// test with two files with both containing invalid EC2 UUID
	ioutil.WriteFile(file1, []byte("garbage"), 0644)
	ioutil.WriteFile(file2, []byte("ec3e1916-9099-7caf-fd21-012345abcdef"), 0644)
	assert.False(t, isEC2Instance([]string{file1, file2}))
	cachedIsEC2Instance = nil
	os.Remove(file1)
	os.Remove(file2)
}

func TestGetAWSMetadata(t *testing.T) {
	sm := http.NewServeMux()
	sm.HandleFunc("/latest/meta-data/instance-id", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "i-12345678")
	})
	sm.HandleFunc("/latest/meta-data/placement/availability-zone", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "us-east-7")
	})

	addr := "localhost:8880"
	ln, err := net.Listen("tcp", addr)
	require.NoError(t, err)

	s := &http.Server{Addr: addr, Handler: sm}
	// change EC2 MD URLs
	origIsEC2 := cachedIsEC2Instance
	True := true
	cachedIsEC2Instance = &True
	ec2MetadataInstanceIDURL = strings.Replace(ec2MetadataInstanceIDURL, "169.254.169.254", addr, 1)
	ec2MetadataZoneURL = strings.Replace(ec2MetadataZoneURL, "169.254.169.254", addr, 1)
	go s.Serve(ln)
	defer func() { // restore old URLs
		ln.Close()
		ec2MetadataInstanceIDURL = strings.Replace(ec2MetadataInstanceIDURL, addr, "169.254.169.254", 1)
		ec2MetadataZoneURL = strings.Replace(ec2MetadataZoneURL, addr, "169.254.169.254", 1)
		cachedIsEC2Instance = origIsEC2
	}()
	time.Sleep(50 * time.Millisecond)

	id := getAWSInstanceID()
	assert.Equal(t, id, "i-12345678")
	zone := getAWSInstanceZone()
	assert.Equal(t, zone, "us-east-7")
}

func TestGetContainerID(t *testing.T) {
	id := getContainerID()
	if getLineByKeyword("/proc/self/cgroup", "/docker/") != "" ||
		getLineByKeyword("/proc/self/cgroup", "/ecs/") != "" {

		assert.NotEmpty(t, id)
		assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]+$`), id)
	} else {
		assert.Empty(t, id)
	}
}

func TestAddMetricsValue(t *testing.T) {
	index := 0
	bbuf := NewBsonBuffer()
	addMetricsValue(bbuf, &index, "name1", int(111))
	addMetricsValue(bbuf, &index, "name2", int64(222))
	addMetricsValue(bbuf, &index, "name3", float32(333.33))
	addMetricsValue(bbuf, &index, "name4", float64(444.44))
	addMetricsValue(bbuf, &index, "name5", "hello")
	bsonBufferFinish(bbuf)
	m := bsonToMap(bbuf)

	assert.NotZero(t, m["0"])
	m2 := m["0"].(map[string]interface{})
	assert.Equal(t, "name1", m2["name"])
	assert.Equal(t, int(111), m2["value"])

	assert.NotZero(t, m["1"])
	m2 = m["1"].(map[string]interface{})
	assert.Equal(t, "name2", m2["name"])
	assert.Equal(t, int64(222), m2["value"])

	assert.NotZero(t, m["2"])
	m2 = m["2"].(map[string]interface{})
	assert.Equal(t, "name3", m2["name"])
	f64 := m2["value"].(float64)
	assert.Equal(t, float64(333.33), round(f64, .5, 2))

	assert.NotZero(t, m["3"])
	m2 = m["3"].(map[string]interface{})
	assert.Equal(t, "name4", m2["name"])
	assert.Equal(t, float64(444.44), m2["value"])

	assert.NotZero(t, m["4"])
	m2 = m["4"].(map[string]interface{})
	assert.Equal(t, "name5", m2["name"])
	assert.Equal(t, "unknown", m2["value"])
}

func TestGetTransactionFromURL(t *testing.T) {
	type record struct {
		url         string
		transaction string
	}
	var test = []record{
		{
			"https://github.com/appoptics/appoptics-apm-go/blob/metrics/reporter.go#L867",
			"/appoptics/appoptics-apm-go",
		},
		{
			"http://github.com/librato",
			"/librato",
		},
		{
			"http://github.com",
			"/",
		},
		{
			"github.com/appoptics/appoptics-apm-go/blob",
			"/appoptics/appoptics-apm-go",
		},
		{
			"github.com:8080/appoptics/appoptics-apm-go/blob",
			"/appoptics/appoptics-apm-go",
		},
		{
			" ",
			"/",
		},
	}

	for _, r := range test {
		assert.Equal(t, r.transaction, getTransactionFromURL(r.url), "url: "+r.url)
	}
}

func TestIsWithinLimit(t *testing.T) {
	m := make(map[string]bool)
	assert.True(t, isWithinLimit(&m, "t1", 3))
	assert.True(t, isWithinLimit(&m, "t2", 3))
	assert.True(t, isWithinLimit(&m, "t3", 3))
	assert.False(t, isWithinLimit(&m, "t4", 3))
	assert.True(t, isWithinLimit(&m, "t2", 3))
}

func TestRecordMeasurement(t *testing.T) {
	var me = &measurements{
		measurements: make(map[string]*measurement),
	}

	t1 := make(map[string]string)
	t1["t1"] = "tag1"
	t1["t2"] = "tag2"
	recordMeasurement(me, "name1", &t1, 111.11, 1, false)
	recordMeasurement(me, "name1", &t1, 222, 1, false)
	assert.NotNil(t, me.measurements["name1&false&t1:tag1&t2:tag2&"])
	m := me.measurements["name1&false&t1:tag1&t2:tag2&"]
	assert.Equal(t, "tag1", m.tags["t1"])
	assert.Equal(t, "tag2", m.tags["t2"])
	assert.Equal(t, 333.11, m.sum)
	assert.Equal(t, 2, m.count)
	assert.False(t, m.reportSum)

	t2 := make(map[string]string)
	t2["t3"] = "tag3"
	recordMeasurement(me, "name2", &t2, 123.456, 3, true)
	assert.NotNil(t, me.measurements["name2&true&t3:tag3&"])
	m = me.measurements["name2&true&t3:tag3&"]
	assert.Equal(t, "tag3", m.tags["t3"])
	assert.Equal(t, 123.456, m.sum)
	assert.Equal(t, 3, m.count)
	assert.True(t, m.reportSum)
}

func TestRecordHistogram(t *testing.T) {
	var hi = &histograms{
		histograms: make(map[string]*histogram),
	}

	recordHistogram(hi, "", time.Duration(123))
	recordHistogram(hi, "", time.Duration(1554))
	assert.NotNil(t, hi.histograms[""])
	h := hi.histograms[""]
	assert.Empty(t, h.tags["TransactionName"])
	encoded, _ := hdrhist.EncodeCompressed(h.hist)
	assert.Equal(t, "HISTFAAAACR42pJpmSzMwMDAxIAKGEHEtclLGOw/QASYmAABAAD//1njBIo=", string(encoded))

	recordHistogram(hi, "hist1", time.Duration(453122))
	assert.NotNil(t, hi.histograms["hist1"])
	h = hi.histograms["hist1"]
	assert.Equal(t, "hist1", h.tags["TransactionName"])
	encoded, _ = hdrhist.EncodeCompressed(h.hist)
	assert.Equal(t, "HISTFAAAACR42pJpmSzMwMDAxIAKGEHEtclLGOw/QAQEmQABAAD//1oBBJk=", string(encoded))

	var buf bytes.Buffer
	log.SetOutput(&buf)
	recordHistogram(hi, "hist2", time.Duration(4531224545454563))
	log.SetOutput(os.Stderr)
	assert.Contains(t, buf.String(), "Failed to record histogram: value to large")
}

func TestAddMeasurementToBSON(t *testing.T) {
	veryLongTagName := "verylongnameAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	veryLongTagValue := "verylongtagAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	veryLongTagNameTrimmed := veryLongTagName[0:64]
	veryLongTagValueTrimmed := veryLongTagValue[0:255]

	tags1 := make(map[string]string)
	tags1["t1"] = "tag1"
	tags2 := make(map[string]string)
	tags2[veryLongTagName] = veryLongTagValue

	measurement1 := &measurement{
		name:      "name1",
		tags:      tags1,
		count:     45,
		sum:       592.42,
		reportSum: false,
	}
	measurement2 := &measurement{
		name:      "name2",
		tags:      tags2,
		count:     777,
		sum:       6530.3,
		reportSum: true,
	}

	index := 0
	bbuf := NewBsonBuffer()
	addMeasurementToBSON(bbuf, &index, measurement1)
	addMeasurementToBSON(bbuf, &index, measurement2)
	bsonBufferFinish(bbuf)
	m := bsonToMap(bbuf)

	assert.NotZero(t, m["0"])
	m1 := m["0"].(map[string]interface{})
	assert.Equal(t, "name1", m1["name"])
	assert.Equal(t, 45, m1["count"])
	assert.Nil(t, m1["sum"])
	assert.NotZero(t, m1["tags"])
	t1 := m1["tags"].(map[string]interface{})
	assert.Equal(t, "tag1", t1["t1"])

	assert.NotZero(t, m["1"])
	m2 := m["1"].(map[string]interface{})
	assert.Equal(t, "name2", m2["name"])
	assert.Equal(t, 777, m2["count"])
	assert.Equal(t, 6530.3, m2["sum"])
	assert.NotZero(t, m2["tags"])
	t2 := m2["tags"].(map[string]interface{})
	assert.Nil(t, t2[veryLongTagName])
	assert.Equal(t, veryLongTagValueTrimmed, t2[veryLongTagNameTrimmed])
}

func TestAddHistogramToBSON(t *testing.T) {
	veryLongTagName := "verylongnameAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	veryLongTagValue := "verylongtagAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" +
		"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
	veryLongTagNameTrimmed := veryLongTagName[0:64]
	veryLongTagValueTrimmed := veryLongTagValue[0:255]

	tags1 := make(map[string]string)
	tags1["t1"] = "tag1"
	tags2 := make(map[string]string)
	tags2[veryLongTagName] = veryLongTagValue

	h1 := &histogram{
		hist: hdrhist.WithConfig(hdrhist.Config{
			LowestDiscernible: 1,
			HighestTrackable:  3600000000,
			SigFigs:           3,
		}),
		tags: tags1,
	}
	h1.hist.Record(34532123)
	h2 := &histogram{
		hist: hdrhist.WithConfig(hdrhist.Config{
			LowestDiscernible: 1,
			HighestTrackable:  3600000000,
			SigFigs:           3,
		}),
		tags: tags2,
	}
	h2.hist.Record(39023)

	index := 0
	bbuf := NewBsonBuffer()
	addHistogramToBSON(bbuf, &index, h1)
	addHistogramToBSON(bbuf, &index, h2)
	bsonBufferFinish(bbuf)
	m := bsonToMap(bbuf)

	assert.NotZero(t, m["0"])
	m1 := m["0"].(map[string]interface{})
	assert.Equal(t, "TransactionResponseTime", m1["name"])
	assert.Equal(t, "HISTFAAAACh42pJpmSzMwMDAwgABzFCaEURcm7yEwf4DRGBnAxMTIAAA//9n9AXI", m1["value"])
	assert.NotZero(t, m1["tags"])
	t1 := m1["tags"].(map[string]interface{})
	assert.Equal(t, "tag1", t1["t1"])

	assert.NotZero(t, m["1"])
	m2 := m["1"].(map[string]interface{})
	assert.Equal(t, "TransactionResponseTime", m2["name"])
	assert.Equal(t, "HISTFAAAACZ42pJpmSzMwMDAzAABMJoRRFybvITB/gNEoDWZCRAAAP//YTIFdA==", m2["value"])
	assert.NotZero(t, m2["tags"])
	t2 := m2["tags"].(map[string]interface{})
	assert.Nil(t, t2[veryLongTagName])
	assert.Equal(t, veryLongTagValueTrimmed, t2[veryLongTagNameTrimmed])
}

func TestGenerateMetricsMessage(t *testing.T) {
	bbuf := &bsonBuffer{
		buf: generateMetricsMessage(15, &eventQueueStats{}),
	}
	m := bsonToMap(bbuf)

	assert.Equal(t, cachedHostname, m["Hostname"])
	assert.Equal(t, getDistro(), m["Distro"])
	assert.Equal(t, cachedPid, m["PID"])
	assert.True(t, m["Timestamp_u"].(int64) > 1509053785684891)
	assert.Equal(t, 15, m["MetricsFlushInterval"])

	mts := m["measurements"].([]interface{})

	type testCase struct {
		name  string
		value interface{}
	}

	// TODO add request counters

	testCases := []testCase{
		{"RequestCount", int64(1)},
		{"TraceCount", int64(1)},
		{"TokenBucketExhaustionCount", int64(1)},
		{"SampleCount", int64(1)},
		{"ThroughTraceCount", int64(1)},
		{"NumSent", int64(1)},
		{"NumOverflowed", int64(1)},
		{"NumFailed", int64(1)},
		{"TotalEvents", int64(1)},
		{"QueueLargest", int64(1)},
	}
	if runtime.GOOS == "linux" {
		testCases = append(testCases, []testCase{
			{"Load1", float64(1)},
			{"TotalRAM", int64(1)},
			{"FreeRAM", int64(1)},
			{"ProcessRAM", int(1)},
		}...)
	}
	testCases = append(testCases, []testCase{
		{"JMX.type=threadcount,name=NumGoroutine", int(1)},
		{"JMX.Memory:MemStats.Alloc", int64(1)},
		{"JMX.Memory:MemStats.TotalAlloc", int64(1)},
		{"JMX.Memory:MemStats.Sys", int64(1)},
		{"JMX.Memory:type=count,name=MemStats.Lookups", int64(1)},
		{"JMX.Memory:type=count,name=MemStats.Mallocs", int64(1)},
		{"JMX.Memory:type=count,name=MemStats.Frees", int64(1)},
		{"JMX.Memory:MemStats.Heap.Alloc", int64(1)},
		{"JMX.Memory:MemStats.Heap.Sys", int64(1)},
		{"JMX.Memory:MemStats.Heap.Idle", int64(1)},
		{"JMX.Memory:MemStats.Heap.Inuse", int64(1)},
		{"JMX.Memory:MemStats.Heap.Released", int64(1)},
		{"JMX.Memory:type=count,name=MemStats.Heap.Objects", int64(1)},
		{"JMX.type=count,name=GCStats.NumGC", int64(1)},
	}...)

	for i, tc := range testCases {
		assert.Equal(t, tc.name, mts[i].(map[string]interface{})["name"])
		assert.IsType(t, mts[i].(map[string]interface{})["value"], tc.value, tc.name)
	}

	assert.Nil(t, m["TransactionNameOverflow"])

	metricsHTTPMeasurements.transactionNameOverflow = true
	bbuf.buf = generateMetricsMessage(15, &eventQueueStats{})
	m = bsonToMap(bbuf)

	assert.NotNil(t, m["TransactionNameOverflow"])
	assert.True(t, m["TransactionNameOverflow"].(bool))
}
