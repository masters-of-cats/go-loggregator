package v2

import (
	"time"

	"code.cloudfoundry.org/go-loggregator/internal/loggregator_v2"
	"github.com/cloudfoundry/sonde-go/events"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
)

type envelopeWithResponseChannel struct {
	envelope *loggregator_v2.Envelope
}

type grpcClient struct {
	batchStreamer BatchStreamer
	sender        loggregator_v2.Ingress_BatchSenderClient
	envelopes     chan *envelopeWithResponseChannel
	jobOpts       JobOpts

	batchMaxSize       uint
	batchFlushInterval time.Duration
}

type JobOpts struct {
	Deployment string
	Name       string
	Index      string
	IP         string
	Origin     string
}

type BatchStreamer interface {
	BatchSender(ctx context.Context, opts ...grpc.CallOption) (loggregator_v2.Ingress_BatchSenderClient, error)
}

type V2Option func(*grpcClient)

func WithJobOpts(j JobOpts) V2Option {
	return func(c *grpcClient) {
		c.jobOpts = j
	}
}

func WithBatchMaxSize(maxSize uint) V2Option {
	return func(c *grpcClient) {
		c.batchMaxSize = maxSize
	}
}

func WithBatchFlushInterval(d time.Duration) V2Option {
	return func(c *grpcClient) {
		c.batchFlushInterval = d
	}
}

func NewClient(b BatchStreamer, opts ...V2Option) (*grpcClient, error) {
	client := &grpcClient{
		batchStreamer:      b,
		envelopes:          make(chan *envelopeWithResponseChannel),
		batchMaxSize:       100,
		batchFlushInterval: time.Second,
	}

	for _, o := range opts {
		o(client)
	}

	go client.startSender()

	return client, nil
}

func (c *grpcClient) SendAppLog(appID, message, sourceType, sourceInstance string) {
	c.send(createLogEnvelope(appID, message, sourceType, sourceInstance, loggregator_v2.Log_OUT))
}

func (c *grpcClient) SendAppErrorLog(appID, message, sourceType, sourceInstance string) {
	c.send(createLogEnvelope(appID, message, sourceType, sourceInstance, loggregator_v2.Log_ERR))
}

func (c *grpcClient) SendAppMetrics(m *events.ContainerMetric) {
	env := &loggregator_v2.Envelope{
		Timestamp: time.Now().UnixNano(),
		SourceId:  m.GetApplicationId(),
		Message: &loggregator_v2.Envelope_Gauge{
			Gauge: &loggregator_v2.Gauge{
				Metrics: map[string]*loggregator_v2.GaugeValue{
					"instance_index": newGaugeValue(float64(m.GetInstanceIndex())),
					"cpu":            newGaugeValue(m.GetCpuPercentage()),
					"memory":         newGaugeValueFromUInt64(m.GetMemoryBytes()),
					"disk":           newGaugeValueFromUInt64(m.GetDiskBytes()),
					"memory_quota":   newGaugeValueFromUInt64(m.GetMemoryBytesQuota()),
					"disk_quota":     newGaugeValueFromUInt64(m.GetDiskBytesQuota()),
				},
			},
		},
	}
	c.send(env)
}

func (c *grpcClient) SendDuration(name string, duration time.Duration) {
	metrics := make(map[string]*loggregator_v2.GaugeValue)
	metrics[name] = &loggregator_v2.GaugeValue{
		Unit:  "nanos",
		Value: float64(duration),
	}
	c.sendGauge(metrics)
}

func (c *grpcClient) SendMebiBytes(name string, mebibytes int) {
	metrics := make(map[string]*loggregator_v2.GaugeValue)
	metrics[name] = &loggregator_v2.GaugeValue{
		Unit:  "MiB",
		Value: float64(mebibytes),
	}
	c.sendGauge(metrics)
}

func (c *grpcClient) SendMetric(name string, value int) {
	metrics := make(map[string]*loggregator_v2.GaugeValue)
	metrics[name] = &loggregator_v2.GaugeValue{
		Unit:  "Metric",
		Value: float64(value),
	}
	c.sendGauge(metrics)
}

func (c *grpcClient) SendBytesPerSecond(name string, value float64) {
	metrics := make(map[string]*loggregator_v2.GaugeValue)
	metrics[name] = &loggregator_v2.GaugeValue{
		Unit:  "B/s",
		Value: float64(value),
	}
	c.sendGauge(metrics)
}

func (c *grpcClient) SendRequestsPerSecond(name string, value float64) {
	metrics := make(map[string]*loggregator_v2.GaugeValue)
	metrics[name] = &loggregator_v2.GaugeValue{
		Unit:  "Req/s",
		Value: float64(value),
	}
	c.sendGauge(metrics)
}

func (c *grpcClient) IncrementCounter(name string) {
	env := &loggregator_v2.Envelope{
		Timestamp: time.Now().UnixNano(),
		Message: &loggregator_v2.Envelope_Counter{
			Counter: &loggregator_v2.Counter{
				Name: name,
				Value: &loggregator_v2.Counter_Delta{
					Delta: uint64(1),
				},
			},
		},
	}

	c.send(env)
}

func (c *grpcClient) startSender() {
	t := time.NewTimer(c.batchFlushInterval)

	var batch []*loggregator_v2.Envelope
	for {
		select {
		case envelopeWithResponseChannel := <-c.envelopes:
			batch = append(batch, envelopeWithResponseChannel.envelope)

			if len(batch) >= int(c.batchMaxSize) {
				c.flush(batch)
				batch = nil
			}

			if !t.Stop() {
				<-t.C
			}
		case <-t.C:
			if len(batch) > 0 {
				c.flush(batch)
				batch = nil
			}
		}
		t.Reset(c.batchFlushInterval)
	}
}

func (c *grpcClient) flush(batch []*loggregator_v2.Envelope) error {
	if c.sender == nil {
		var err error
		c.sender, err = c.batchStreamer.BatchSender(context.TODO())
		if err != nil {
			return err
		}
	}

	err := c.sender.Send(&loggregator_v2.EnvelopeBatch{Batch: batch})
	if err != nil {
		c.sender = nil
		return err
	}

	return nil
}

func (c *grpcClient) send(envelope *loggregator_v2.Envelope) {
	if envelope.Tags == nil {
		envelope.Tags = make(map[string]*loggregator_v2.Value)
	}
	envelope.Tags["deployment"] = newTextValue(c.jobOpts.Deployment)
	envelope.Tags["job"] = newTextValue(c.jobOpts.Name)
	envelope.Tags["index"] = newTextValue(c.jobOpts.Index)
	envelope.Tags["ip"] = newTextValue(c.jobOpts.IP)
	envelope.Tags["origin"] = newTextValue(c.jobOpts.Origin)

	e := &envelopeWithResponseChannel{
		envelope: envelope,
	}

	c.envelopes <- e
}

func (c *grpcClient) sendGauge(metrics map[string]*loggregator_v2.GaugeValue) {
	c.send(&loggregator_v2.Envelope{
		Timestamp: time.Now().UnixNano(),
		Message: &loggregator_v2.Envelope_Gauge{
			Gauge: &loggregator_v2.Gauge{
				Metrics: metrics,
			},
		},
	})
}