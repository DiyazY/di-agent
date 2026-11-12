package agentmain

import (
	"testing"
	"time"
)

func TestSampleRequestWithoutTimestampTakesTheRequestTime(t *testing.T) {
	now := time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)
	s, err := sampleRequestToTypes(&MetricSampleRequest{NodeID: "master", MetricType: "cpu_utilization", Value: 0.5}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.TimestampUnix != now.Unix() {
		t.Errorf("TimestampUnix = %d, want %d", s.TimestampUnix, now.Unix())
	}
	s, err = sampleRequestToTypes(&MetricSampleRequest{NodeID: "master", MetricType: "cpu_utilization", Value: 0.5, TimestampUnix: 1700000000}, nil, now)
	if err != nil {
		t.Fatal(err)
	}
	if s.TimestampUnix != 1700000000 {
		t.Errorf("explicit TimestampUnix = %d, want 1700000000", s.TimestampUnix)
	}
}
