package stream

import (
	"bufio"
	"fmt"
	"io"
	"sync"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/bytesutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/flagutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/protoparser/protoparserutil"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/protoparser/vmimport"
	"github.com/VictoriaMetrics/VictoriaMetrics/lib/writeconcurrencylimiter"
	"github.com/VictoriaMetrics/metrics"
)

var maxLineLen = flagutil.NewBytes("import.maxLineLen", 10*1024*1024, "The maximum length in bytes of a single line accepted by /api/v1/import; "+
	"the line length can be limited with 'max_rows_per_line' query arg passed to /api/v1/export")

// Parse parses /api/v1/import lines from req and calls callback for the parsed rows.
//
// The callback can be called concurrently multiple times for streamed data from reader.
//
// callback shouldn't hold rows after returning.
func Parse(r io.Reader, encoding string, callback func(rows []vmimport.Row) error) error {
	reader, err := protoparserutil.GetUncompressedReader(r, encoding)
	if err != nil {
		return fmt.Errorf("cannot decode vmimport data: %w", err)
	}
	defer protoparserutil.PutUncompressedReader(reader)

	wcr := writeconcurrencylimiter.GetReader(reader)
	defer writeconcurrencylimiter.PutReader(wcr)
	reader = wcr

	ctx := getStreamContext(reader)
	defer putStreamContext(ctx)
	for ctx.Read() {
		uw := getUnmarshalWork()
		uw.ctx = ctx
		uw.callback = callback
		uw.reqBuf, ctx.reqBuf = ctx.reqBuf, uw.reqBuf
		ctx.wg.Add(1)
		protoparserutil.ScheduleUnmarshalWork(uw)
		wcr.DecConcurrency()
	}
	ctx.wg.Wait()
	if err := ctx.Error(); err != nil {
		return err
	}
	return ctx.callbackErr
}

func (ctx *streamContext) Read() bool {
	readCalls.Inc()
	if ctx.err != nil || ctx.hasCallbackError() {
		return false
	}
	ctx.reqBuf, ctx.tailBuf, ctx.err = protoparserutil.ReadLinesBlockExt(ctx.br, ctx.reqBuf, ctx.tailBuf, maxLineLen.IntN())
	if ctx.err != nil {
		if ctx.err != io.EOF {
			readErrors.Inc()
			ctx.err = fmt.Errorf("cannot read vmimport data: %w", ctx.err)
		}
		return false
	}
	return true
}

var (
	readCalls  = metrics.NewCounter(`vm_protoparser_read_calls_total{type="vmimport"}`)
	readErrors = metrics.NewCounter(`vm_protoparser_read_errors_total{type="vmimport"}`)
	rowsRead   = metrics.NewCounter(`vm_protoparser_rows_read_total{type="vmimport"}`)
)

type streamContext struct {
	br      *bufio.Reader
	reqBuf  []byte
	tailBuf []byte
	err     error

	wg              sync.WaitGroup
	callbackErrLock sync.Mutex
	callbackErr     error
}

func (ctx *streamContext) Error() error {
	if ctx.err == io.EOF {
		return nil
	}
	return ctx.err
}

func (ctx *streamContext) hasCallbackError() bool {
	ctx.callbackErrLock.Lock()
	ok := ctx.callbackErr != nil
	ctx.callbackErrLock.Unlock()
	return ok
}

func (ctx *streamContext) reset() {
	ctx.br.Reset(nil)
	ctx.reqBuf = ctx.reqBuf[:0]
	ctx.tailBuf = ctx.tailBuf[:0]
	ctx.err = nil
	ctx.callbackErr = nil
}

func getStreamContext(r io.Reader) *streamContext {
	if v := streamContextPool.Get(); v != nil {
		ctx := v.(*streamContext)
		ctx.br.Reset(r)
		return ctx
	}
	return &streamContext{
		br: bufio.NewReaderSize(r, 64*1024),
	}
}

func putStreamContext(ctx *streamContext) {
	ctx.reset()
	streamContextPool.Put(ctx)
}

var streamContextPool sync.Pool

type unmarshalWork struct {
	rows     vmimport.Rows
	ctx      *streamContext
	callback func(rows []vmimport.Row) error
	reqBuf   []byte
}

func (uw *unmarshalWork) reset() {
	uw.rows.Reset()
	uw.ctx = nil
	uw.callback = nil
	uw.reqBuf = uw.reqBuf[:0]
}

func (uw *unmarshalWork) runCallback(rows []vmimport.Row) {
	ctx := uw.ctx
	if err := uw.callback(rows); err != nil {
		ctx.callbackErrLock.Lock()
		if ctx.callbackErr == nil {
			ctx.callbackErr = fmt.Errorf("error when processing imported data: %w", err)
		}
		ctx.callbackErrLock.Unlock()
	}
	ctx.wg.Done()
}

// Unmarshal implements proroparserutil.UnmarshalWork
func (uw *unmarshalWork) Unmarshal() {
	uw.rows.Unmarshal(bytesutil.ToUnsafeString(uw.reqBuf))
	rows := uw.rows.Rows
	for i := range rows {
		row := &rows[i]
		rowsRead.Add(len(row.Timestamps))
	}
	uw.runCallback(rows)
	putUnmarshalWork(uw)
}

func getUnmarshalWork() *unmarshalWork {
	v := unmarshalWorkPool.Get()
	if v == nil {
		return &unmarshalWork{}
	}
	return v.(*unmarshalWork)
}

func putUnmarshalWork(uw *unmarshalWork) {
	uw.reset()
	unmarshalWorkPool.Put(uw)
}

var unmarshalWorkPool sync.Pool
