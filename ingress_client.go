package loggregator

import (
	"crypto/tls"
	"fmt"
	"io/ioutil"
	"log"
	"time"

	"github.com/cloudfoundry/sonde-go/events"
	"github.com/gogo/protobuf/proto"
	"golang.org/x/net/context"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"code.cloudfoundry.org/go-loggregator/rpc/loggregator_v2"
)

// IngressOption is the type of a configurable client option.
type IngressOption func(*IngressClient)

// WithTag allows for the configuration of arbitrary string value
// metadata which will be included in all data sent to Loggregator
func WithTag(name, value string) IngressOption {
	return func(c *IngressClient) {
		c.tags[name] = value
	}
}

// WithBatchMaxSize allows for the configuration of the number of messages to
// collect before emitting them into loggregator. By default, its value is 100
// messages.
//
// Note that aside from batch size, messages will be flushed from
// the client into loggregator at a fixed interval to ensure messages are not
// held for an undue amount of time before being sent. In other words, even if
// the client has not yet achieved the maximum batch size, the batch interval
// may trigger the messages to be sent.
func WithBatchMaxSize(maxSize uint) IngressOption {
	return func(c *IngressClient) {
		c.batchMaxSize = maxSize
	}
}

// WithBatchFlushInterval allows for the configuration of the maximum time to
// wait before sending a batch of messages. Note that the batch interval
// may be triggered prior to the batch reaching the configured maximum size.
func WithBatchFlushInterval(d time.Duration) IngressOption {
	return func(c *IngressClient) {
		c.batchFlushInterval = d
	}
}

// WithAddr allows for the configuration of the loggregator v2 address.
// The value to defaults to localhost:3458, which happens to be the default
// address in the loggregator server.
func WithAddr(addr string) IngressOption {
	return func(c *IngressClient) {
		c.addr = addr
	}
}

// Logger declares the minimal logging interface used within the v2 client
type Logger interface {
	Printf(string, ...interface{})
}

// WithLogger allows for the configuration of a logger.
// By default, the logger is disabled.
func WithLogger(l Logger) IngressOption {
	return func(c *IngressClient) {
		c.logger = l
	}
}

// IngressClient represents an emitter into loggregator. It should be created with the
// NewIngressClient constructor.
type IngressClient struct {
	client loggregator_v2.IngressClient
	sender loggregator_v2.Ingress_BatchSenderClient

	envelopes chan *loggregator_v2.Envelope
	tags      map[string]string

	batchMaxSize       uint
	batchFlushInterval time.Duration
	addr               string

	logger Logger

	closeErrors chan error
}

// NewIngressClient creates a v2 loggregator client. Its TLS configuration
// must share a CA with the loggregator server.
func NewIngressClient(tlsConfig *tls.Config, opts ...IngressOption) (*IngressClient, error) {
	c := &IngressClient{
		envelopes:          make(chan *loggregator_v2.Envelope, 100),
		tags:               make(map[string]string),
		batchMaxSize:       100,
		batchFlushInterval: time.Second,
		addr:               "localhost:3458",
		logger:             log.New(ioutil.Discard, "", 0),
		closeErrors:        make(chan error),
	}

	for _, o := range opts {
		o(c)
	}

	conn, err := grpc.Dial(
		c.addr,
		grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)),
	)
	if err != nil {
		return nil, err
	}
	c.client = loggregator_v2.NewIngressClient(conn)

	go c.startSender()

	return c, nil
}

// EnvelopeWrapper is used to setup v1 Envelopes. It should not be created or
// used by a user.
type EnvelopeWrapper struct {
	proto.Message

	Messages []*events.Envelope
	Tags     map[string]string
}

// EmitLogOption is the option type passed into EmitLog
type EmitLogOption func(proto.Message)

// WithAppInfo configures the meta data associated with emitted data
func WithAppInfo(appID, sourceType, sourceInstance string) EmitLogOption {
	return func(m proto.Message) {
		switch e := m.(type) {
		case *loggregator_v2.Envelope:
			e.SourceId = appID
			e.InstanceId = sourceInstance
			e.Tags["source_type"] = sourceType
		case *EnvelopeWrapper:
			e.Messages[0].GetLogMessage().AppId = proto.String(appID)
			e.Messages[0].GetLogMessage().SourceType = proto.String(sourceType)
			e.Messages[0].GetLogMessage().SourceInstance = proto.String(sourceInstance)
		default:
			panic(fmt.Sprintf("unsupported Message type: %T", m))
		}
	}
}

// WithStdout sets the output type to stdout. Without using this option,
// all data is assumed to be stderr output.
func WithStdout() EmitLogOption {
	return func(m proto.Message) {
		switch e := m.(type) {
		case *loggregator_v2.Envelope:
			e.GetLog().Type = loggregator_v2.Log_OUT
		case *EnvelopeWrapper:
			e.Messages[0].GetLogMessage().MessageType = events.LogMessage_OUT.Enum()
		default:
			panic(fmt.Sprintf("unsupported Message type: %T", m))
		}
	}
}

// EmitLog sends a message to loggregator.
func (c *IngressClient) EmitLog(message string, opts ...EmitLogOption) {
	e := &loggregator_v2.Envelope{
		Timestamp: time.Now().UnixNano(),
		Message: &loggregator_v2.Envelope_Log{
			Log: &loggregator_v2.Log{
				Payload: []byte(message),
				Type:    loggregator_v2.Log_ERR,
			},
		},
		Tags: make(map[string]string),
	}

	for k, v := range c.tags {
		e.Tags[k] = v
	}

	for _, o := range opts {
		o(e)
	}

	c.envelopes <- e
}

// EmitGaugeOption is the option type passed into EmitGauge
type EmitGaugeOption func(proto.Message)

// WithGaugeAppInfo configures an ID associated with the gauge
func WithGaugeAppInfo(appID string) EmitGaugeOption {
	return func(m proto.Message) {
		switch e := m.(type) {
		case *loggregator_v2.Envelope:
			e.SourceId = appID
		case *EnvelopeWrapper:
			e.Tags["source_id"] = appID
		default:
			panic(fmt.Sprintf("unsupported Message type: %T", m))
		}
	}
}

// WithGaugeValue adds a gauge information. For example,
// to send information about current CPU usage, one might use:
//
// WithGaugeValue("cpu", 3.0, "percent")
//
// An number of calls to WithGaugeValue may be passed into EmitGauge.
// If there are duplicate names in any of the options, i.e., "cpu" and "cpu",
// then the last EmitGaugeOption will take precedence.
func WithGaugeValue(name string, value float64, unit string) EmitGaugeOption {
	return func(m proto.Message) {
		switch e := m.(type) {
		case *loggregator_v2.Envelope:
			e.GetGauge().Metrics[name] = &loggregator_v2.GaugeValue{Value: value, Unit: unit}
		case *EnvelopeWrapper:
			e.Messages = append(e.Messages, &events.Envelope{
				ValueMetric: &events.ValueMetric{
					Name:  proto.String(name),
					Value: proto.Float64(value),
					Unit:  proto.String(unit),
				},
			})
		default:
			panic(fmt.Sprintf("unsupported Message type: %T", m))
		}
	}
}

// EmitGauge sends the configured gauge values to loggregator.
// If no EmitGaugeOption values are present, the client will emit
// an empty gauge.
func (c *IngressClient) EmitGauge(opts ...EmitGaugeOption) {
	e := &loggregator_v2.Envelope{
		Timestamp: time.Now().UnixNano(),
		Message: &loggregator_v2.Envelope_Gauge{
			Gauge: &loggregator_v2.Gauge{
				Metrics: make(map[string]*loggregator_v2.GaugeValue),
			},
		},
		Tags: make(map[string]string),
	}

	for k, v := range c.tags {
		e.Tags[k] = v
	}

	for _, o := range opts {
		o(e)
	}

	c.envelopes <- e
}

// EmitCounterOption is the option type passed into EmitCounter.
type EmitCounterOption func(proto.Message)

// WithDelta is an option that sets the delta for a counter.
func WithDelta(d uint64) EmitCounterOption {
	return func(m proto.Message) {
		switch e := m.(type) {
		case *loggregator_v2.Envelope:
			e.GetCounter().Value = &loggregator_v2.Counter_Delta{Delta: d}
		case *EnvelopeWrapper:
			e.Messages[0].GetCounterEvent().Delta = proto.Uint64(d)
		default:
			panic(fmt.Sprintf("unsupported Message type: %T", m))
		}
	}
}

// CloseSend will flush the envelope buffers and close the stream to the
// ingress server. This method will block until the buffers are flushed.
func (c *IngressClient) CloseSend() error {
	close(c.envelopes)

	return <-c.closeErrors
}

// EmitCounter sends a counter envelope with a delta of 1.
func (c *IngressClient) EmitCounter(name string, opts ...EmitCounterOption) {
	e := &loggregator_v2.Envelope{
		Timestamp: time.Now().UnixNano(),
		Message: &loggregator_v2.Envelope_Counter{
			Counter: &loggregator_v2.Counter{
				Name: name,
				Value: &loggregator_v2.Counter_Delta{
					Delta: uint64(1),
				},
			},
		},
		Tags: make(map[string]string),
	}

	for k, v := range c.tags {
		e.Tags[k] = v
	}

	for _, o := range opts {
		o(e)
	}

	c.envelopes <- e
}

func (c *IngressClient) startSender() {
	t := time.NewTimer(c.batchFlushInterval)

	var batch []*loggregator_v2.Envelope
	for {
		select {
		case env := <-c.envelopes:
			if env == nil {
				c.closeErrors <- c.flush(batch, true)
				return
			}

			batch = append(batch, env)

			if len(batch) >= int(c.batchMaxSize) {
				c.flush(batch, false)
				batch = nil
				if !t.Stop() {
					<-t.C
				}
				t.Reset(c.batchFlushInterval)
			}
		case <-t.C:
			if len(batch) > 0 {
				c.flush(batch, false)
				batch = nil
			}
			t.Reset(c.batchFlushInterval)
		}
	}
}

func (c *IngressClient) flush(batch []*loggregator_v2.Envelope, close bool) error {
	err := c.emit(batch, close)
	if err != nil {
		c.logger.Printf("Error while flushing: %s", err)
	}

	return err
}

func (c *IngressClient) emit(batch []*loggregator_v2.Envelope, close bool) error {
	if c.sender == nil {
		var err error
		// TODO Callers of emit should pass in a context. The code should not
		// be hard-coding context.TODO here.
		c.sender, err = c.client.BatchSender(context.TODO())
		if err != nil {
			return err
		}
	}

	err := c.sender.Send(&loggregator_v2.EnvelopeBatch{Batch: batch})
	if err != nil {
		c.sender = nil
		return err
	}

	if close {
		return c.sender.CloseSend()
	}

	return nil
}

// WithEnvelopeTag adds a tag to the envelope.
func WithEnvelopeTag(name, value string) func(proto.Message) {
	return func(m proto.Message) {
		switch e := m.(type) {
		case *loggregator_v2.Envelope:
			e.Tags[name] = value
		case *EnvelopeWrapper:
			e.Tags[name] = value
		default:
			panic(fmt.Sprintf("unsupported Message type: %T", m))
		}
	}
}

// WithEnvelopeTags adds tag information that can be text, integer, or decimal to
// the envelope.  WithEnvelopeTags expects a single call with a complete map
// and will overwrite if called a second time.
func WithEnvelopeTags(tags map[string]string) func(proto.Message) {
	return func(m proto.Message) {
		switch e := m.(type) {
		case *loggregator_v2.Envelope:
			for name, value := range tags {
				e.Tags[name] = value
			}
		case *EnvelopeWrapper:
			for name, value := range tags {
				e.Tags[name] = value
			}
		default:
			panic(fmt.Sprintf("unsupported Message type: %T", m))
		}
	}
}
