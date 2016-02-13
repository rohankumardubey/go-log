package standardtracer

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/opentracing/opentracing-go"
)

type splitTextPropagator struct {
	tracer *tracerImpl
}
type splitBinaryPropagator struct {
	tracer *tracerImpl
}
type goHTTPPropagator struct {
	splitBinaryPropagator
}

const (
	fieldNameTraceID = "traceid"
	fieldNameSpanID  = "spanid"
	fieldNameSampled = "sampled"
)

func (p splitTextPropagator) InjectSpan(
	sp opentracing.Span,
	carrier interface{},
) error {
	sc := sp.(*spanImpl).raw.StandardContext
	splitTextCarrier, ok := carrier.(*opentracing.SplitTextCarrier)
	if !ok {
		return opentracing.InvalidCarrier
	}
	splitTextCarrier.TracerState = map[string]string{
		fieldNameTraceID: strconv.FormatInt(sc.TraceID, 10),
		fieldNameSpanID:  strconv.FormatInt(sc.SpanID, 10),
		fieldNameSampled: strconv.FormatBool(sc.Sampled),
	}
	sc.attrMu.RLock()
	splitTextCarrier.TraceAttributes = make(map[string]string, len(sc.traceAttrs))
	for k, v := range sc.traceAttrs {
		splitTextCarrier.TraceAttributes[k] = v
	}
	sc.attrMu.RUnlock()
	return nil
}

func (p splitTextPropagator) JoinTrace(
	operationName string,
	carrier interface{},
) (opentracing.Span, error) {
	splitTextCarrier, ok := carrier.(*opentracing.SplitTextCarrier)
	if !ok {
		return nil, opentracing.InvalidCarrier
	}
	requiredFieldCount := 0
	var traceID, propagatedSpanID int64
	var sampled bool
	var err error
	for k, v := range splitTextCarrier.TracerState {
		switch strings.ToLower(k) {
		case fieldNameTraceID:
			traceID, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, opentracing.TraceCorrupted
			}
			requiredFieldCount++
		case fieldNameSpanID:
			propagatedSpanID, err = strconv.ParseInt(v, 10, 64)
			if err != nil {
				return nil, opentracing.TraceCorrupted
			}
			requiredFieldCount++
		case fieldNameSampled:
			sampled, err = strconv.ParseBool(v)
			if err != nil {
				return nil, opentracing.TraceCorrupted
			}
			requiredFieldCount++
		default:
			return nil, fmt.Errorf("Unknown TracerState field: %v", k)
		}
	}
	if requiredFieldCount < 3 {
		return nil, fmt.Errorf("Only found %v of 3 required fields", requiredFieldCount)
	}

	return p.tracer.startSpanInternal(
		&StandardContext{
			TraceID:      traceID,
			SpanID:       randomID(),
			ParentSpanID: propagatedSpanID,
			Sampled:      sampled,
			traceAttrs:   splitTextCarrier.TraceAttributes,
		},
		operationName,
		time.Now(),
		opentracing.Tags{},
	), nil
}

func (p splitBinaryPropagator) InjectSpan(
	sp opentracing.Span,
	carrier interface{},
) error {
	sc := sp.(*spanImpl).raw.StandardContext
	splitBinaryCarrier, ok := carrier.(*opentracing.SplitBinaryCarrier)
	if !ok {
		return opentracing.InvalidCarrier
	}
	var err error
	var sampledByte byte
	if sc.Sampled {
		sampledByte = 1
	}

	// Handle the trace and span ids, and sampled status.
	contextBuf := new(bytes.Buffer)
	err = binary.Write(contextBuf, binary.BigEndian, sc.TraceID)
	if err != nil {
		return err
	}

	err = binary.Write(contextBuf, binary.BigEndian, sc.SpanID)
	if err != nil {
		return err
	}

	err = binary.Write(contextBuf, binary.BigEndian, sampledByte)
	if err != nil {
		return err
	}

	// Handle the attributes.
	attrsBuf := new(bytes.Buffer)
	err = binary.Write(attrsBuf, binary.BigEndian, int32(len(sc.traceAttrs)))
	if err != nil {
		return err
	}
	for k, v := range sc.traceAttrs {
		keyBytes := []byte(k)
		err = binary.Write(attrsBuf, binary.BigEndian, int32(len(keyBytes)))
		err = binary.Write(attrsBuf, binary.BigEndian, keyBytes)
		valBytes := []byte(v)
		err = binary.Write(attrsBuf, binary.BigEndian, int32(len(valBytes)))
		err = binary.Write(attrsBuf, binary.BigEndian, valBytes)
	}

	splitBinaryCarrier.TracerState = contextBuf.Bytes()
	splitBinaryCarrier.TraceAttributes = attrsBuf.Bytes()
	return nil
}

func (p splitBinaryPropagator) JoinTrace(
	operationName string,
	carrier interface{},
) (opentracing.Span, error) {
	var err error
	splitBinaryCarrier, ok := carrier.(*opentracing.SplitBinaryCarrier)
	if !ok {
		return nil, opentracing.InvalidCarrier
	}
	// Handle the trace, span ids, and sampled status.
	contextReader := bytes.NewReader(splitBinaryCarrier.TracerState)
	var traceID, propagatedSpanID int64
	var sampledByte byte

	err = binary.Read(contextReader, binary.BigEndian, &traceID)
	if err != nil {
		return nil, opentracing.TraceCorrupted
	}
	err = binary.Read(contextReader, binary.BigEndian, &propagatedSpanID)
	if err != nil {
		return nil, opentracing.TraceCorrupted
	}
	err = binary.Read(contextReader, binary.BigEndian, &sampledByte)
	if err != nil {
		return nil, opentracing.TraceCorrupted
	}

	// Handle the attributes.
	attrsReader := bytes.NewReader(splitBinaryCarrier.TraceAttributes)
	var numAttrs int32
	err = binary.Read(attrsReader, binary.BigEndian, &numAttrs)
	if err != nil {
		return nil, opentracing.TraceCorrupted
	}
	iNumAttrs := int(numAttrs)
	attrMap := make(map[string]string, iNumAttrs)
	for i := 0; i < iNumAttrs; i++ {
		var keyLen int32
		err = binary.Read(attrsReader, binary.BigEndian, &keyLen)
		if err != nil {
			return nil, opentracing.TraceCorrupted
		}
		keyBytes := make([]byte, keyLen)
		err = binary.Read(attrsReader, binary.BigEndian, &keyBytes)
		if err != nil {
			return nil, opentracing.TraceCorrupted
		}

		var valLen int32
		err = binary.Read(attrsReader, binary.BigEndian, &valLen)
		if err != nil {
			return nil, opentracing.TraceCorrupted
		}
		valBytes := make([]byte, valLen)
		err = binary.Read(attrsReader, binary.BigEndian, &valBytes)
		if err != nil {
			return nil, opentracing.TraceCorrupted
		}

		attrMap[string(keyBytes)] = string(valBytes)
	}

	return p.tracer.startSpanInternal(
		&StandardContext{
			TraceID:      traceID,
			SpanID:       randomID(),
			ParentSpanID: propagatedSpanID,
			Sampled:      sampledByte != 0,
			traceAttrs:   attrMap,
		},
		operationName,
		time.Now(),
		opentracing.Tags{},
	), nil
}

const (
	tracerStateHeaderName = "Tracer-State"
	traceAttrsHeaderName  = "Trace-Attributes"
)

func (p goHTTPPropagator) InjectSpan(
	sp opentracing.Span,
	carrier interface{},
) error {
	// Defer to SplitBinary for the real work.
	splitBinaryCarrier := opentracing.NewSplitBinaryCarrier()
	if err := p.splitBinaryPropagator.InjectSpan(sp, splitBinaryCarrier); err != nil {
		return err
	}

	// Encode into the HTTP header as two base64 strings.
	header := carrier.(http.Header)
	header.Add(tracerStateHeaderName, base64.StdEncoding.EncodeToString(
		splitBinaryCarrier.TracerState))
	header.Add(traceAttrsHeaderName, base64.StdEncoding.EncodeToString(
		splitBinaryCarrier.TraceAttributes))

	return nil
}

func (p goHTTPPropagator) JoinTrace(
	operationName string,
	carrier interface{},
) (opentracing.Span, error) {
	// Decode the two base64-encoded data blobs from the HTTP header.
	header := carrier.(http.Header)
	tracerStateBase64, found := header[http.CanonicalHeaderKey(tracerStateHeaderName)]
	if !found || len(tracerStateBase64) == 0 {
		return nil, opentracing.TraceNotFound
	}
	traceAttrsBase64, found := header[http.CanonicalHeaderKey(traceAttrsHeaderName)]
	if !found || len(traceAttrsBase64) == 0 {
		return nil, opentracing.TraceNotFound
	}
	tracerStateBinary, err := base64.StdEncoding.DecodeString(tracerStateBase64[0])
	if err != nil {
		return nil, opentracing.TraceCorrupted
	}
	traceAttrsBinary, err := base64.StdEncoding.DecodeString(traceAttrsBase64[0])
	if err != nil {
		return nil, opentracing.TraceCorrupted
	}

	// Defer to SplitBinary for the real work.
	splitBinaryCarrier := &opentracing.SplitBinaryCarrier{
		TracerState:     tracerStateBinary,
		TraceAttributes: traceAttrsBinary,
	}
	return p.splitBinaryPropagator.JoinTrace(operationName, splitBinaryCarrier)
}