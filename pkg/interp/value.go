package interp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"math/big"
	"strings"

	"github.com/mitchellh/mapstructure"
	"github.com/wader/fq/internal/gojqextra"
	"github.com/wader/fq/internal/ioextra"
	"github.com/wader/fq/pkg/bitio"
	"github.com/wader/fq/pkg/decode"

	"github.com/wader/gojq"
)

func init() {
	functionRegisterFns = append(functionRegisterFns, func(i *Interp) []Function {
		return []Function{
			{"_decode", 2, 2, i._decode, nil},
			{"_is_decode_value", 0, 0, i._isDecodeValue, nil},
			{"_tovalue", 1, 1, i._toValue, nil},
		}
	})
}

type expectedExtkeyError struct {
	Key string
}

func (err expectedExtkeyError) Error() string {
	return "expected a extkey but got: " + err.Key
}

type notUpdateableError struct {
	Typ string
	Key string
}

func (err notUpdateableError) Error() string {
	return fmt.Sprintf("cannot update key %s for %s", err.Key, err.Typ)
}

// TODO: redo/rename
// used by _isDecodeValue
type DecodeValue interface {
	Value
	ToBufferView

	DecodeValue() *decode.Value
}

func (i *Interp) _toValue(c interface{}, a []interface{}) interface{} {
	v, _ := toValue(
		func() Options { return i.Options(a[0]) },
		c,
	)
	return v
}

func (i *Interp) _decode(c interface{}, a []interface{}) interface{} {
	var opts struct {
		Filename string                 `mapstructure:"filename"`
		Force    bool                   `mapstructure:"force"`
		Progress string                 `mapstructure:"_progress"`
		Remain   map[string]interface{} `mapstructure:",remain"`
	}
	_ = mapstructure.Decode(a[1], &opts)

	// TODO: progress hack
	// would be nice to move all progress code into decode but it might be
	// tricky to keep track of absolute positions in the underlaying readers
	// when it uses BitBuf slices, maybe only in Pos()?
	if bbf, ok := c.(*openFile); ok {
		opts.Filename = bbf.filename

		if opts.Progress != "" {
			evalProgress := func(c interface{}) {
				// {approx_read_bytes: 123, total_size: 123} | opts.Progress
				_, _ = i.EvalFuncValues(
					i.evalContext.ctx,
					c,
					opts.Progress,
					nil,
					ioextra.DiscardCtxWriter{Ctx: i.evalContext.ctx},
				)
			}
			bbf.progressFn = func(approxReadBytes, totalSize int64) {
				evalProgress(
					map[string]interface{}{
						"approx_read_bytes": approxReadBytes,
						"total_size":        totalSize,
					},
				)
			}
			// when done decoding, tell progress function were done and disable it
			defer func() {
				bbf.progressFn = nil
				evalProgress(nil)
			}()
		}
	}

	bv, err := toBufferView(c)
	if err != nil {
		return err
	}

	formatName, err := toString(a[0])
	if err != nil {
		return fmt.Errorf("%s: %w", formatName, err)
	}
	decodeFormat, err := i.registry.Group(formatName)
	if err != nil {
		return fmt.Errorf("%s: %w", formatName, err)
	}

	dv, _, err := decode.Decode(i.evalContext.ctx, bv.bb, decodeFormat,
		decode.Options{
			IsRoot:        true,
			FillGaps:      true,
			Force:         opts.Force,
			Range:         bv.r,
			Description:   opts.Filename,
			FormatOptions: opts.Remain,
		},
	)
	if dv == nil {
		var decodeFormatsErr decode.FormatsError
		if errors.As(err, &decodeFormatsErr) {
			var vs []interface{}
			for _, fe := range decodeFormatsErr.Errs {
				vs = append(vs, fe.Value())
			}

			return valueError{vs}
		}

		return valueError{err}
	}

	return makeDecodeValue(dv)
}

func (i *Interp) _isDecodeValue(c interface{}, a []interface{}) interface{} {
	_, ok := c.(DecodeValue)
	return ok
}

func valueKey(name string, a, b func(name string) interface{}) interface{} {
	if strings.HasPrefix(name, "_") {
		return a(name)
	}
	return b(name)
}
func valueHas(key interface{}, a func(name string) interface{}, b func(key interface{}) interface{}) interface{} {
	stringKey, ok := key.(string)
	if ok && strings.HasPrefix(stringKey, "_") {
		if err, ok := a(stringKey).(error); ok {
			return err
		}
		return true
	}
	return b(key)
}

// optsFn is a function as toValue is used by tovalue/0 so needs to be fast
func toValue(optsFn func() Options, v interface{}) (interface{}, bool) {
	switch v := v.(type) {
	case JQValueEx:
		return v.JQValueToGoJQEx(optsFn), true
	case gojq.JQValue:
		return v.JQValueToGoJQ(), true
	case nil, bool, float64, int, string, *big.Int, map[string]interface{}, []interface{}:
		return v, true
	default:
		return nil, false
	}
}

func makeDecodeValue(dv *decode.Value) interface{} {
	switch vv := dv.V.(type) {
	case decode.Compound:
		if vv.IsArray {
			return NewArrayDecodeValue(dv, vv)
		}
		return NewStructDecodeValue(dv, vv)
	case decode.Scalar:
		switch vv := vv.Value().(type) {
		case *bitio.Buffer:
			buf := &bytes.Buffer{}
			if _, err := io.Copy(buf, vv.Copy()); err != nil {
				return err
			}
			// TODO: split *bitio.Buffer into just marker (bit range in root bitbuf)
			// or *bitio.Buffer if actually other bitbuf
			return decodeValue{
				JQValue:         gojqextra.String(buf.String()),
				decodeValueBase: decodeValueBase{dv},
				bitsFormat:      true,
			}
		case bool:
			return decodeValue{
				JQValue:         gojqextra.Boolean(vv),
				decodeValueBase: decodeValueBase{dv},
			}
		case int:
			return decodeValue{
				JQValue:         gojqextra.Number{V: vv},
				decodeValueBase: decodeValueBase{dv},
			}
		case int64:
			return decodeValue{
				JQValue:         gojqextra.Number{V: big.NewInt(vv)},
				decodeValueBase: decodeValueBase{dv},
			}
		case uint64:
			return decodeValue{
				JQValue:         gojqextra.Number{V: new(big.Int).SetUint64(vv)},
				decodeValueBase: decodeValueBase{dv},
			}
		case float64:
			return decodeValue{
				JQValue:         gojqextra.Number{V: vv},
				decodeValueBase: decodeValueBase{dv},
			}
		case string:
			return decodeValue{
				JQValue:         gojqextra.String(vv),
				decodeValueBase: decodeValueBase{dv},
			}
		case []byte:
			// TODO: not sure about this
			// TODO: only synthentic value without range?
			return newBufferRangeFromBuffer(bitio.NewBufferFromBytes(vv, -1), 8)
		case []interface{}:
			return decodeValue{
				JQValue:         gojqextra.Array(vv),
				decodeValueBase: decodeValueBase{dv},
			}
		case map[string]interface{}:
			return decodeValue{
				JQValue:         gojqextra.Object(vv),
				decodeValueBase: decodeValueBase{dv},
			}
		case nil:
			return decodeValue{
				JQValue:         gojqextra.Null{},
				decodeValueBase: decodeValueBase{dv},
			}
		default:
			panic("unreachable")
		}
	default:
		panic("unreachable")
	}
}

type decodeValueBase struct {
	dv *decode.Value
}

func (dvb decodeValueBase) DecodeValue() *decode.Value {
	return dvb.dv
}

func (dvb decodeValueBase) Display(w io.Writer, opts Options) error { return dump(dvb.dv, w, opts) }
func (dvb decodeValueBase) ToBufferView() (BufferRange, error) {
	return BufferRange{bb: dvb.dv.RootBitBuf, r: dvb.dv.InnerRange(), unit: 8}, nil
}
func (dvb decodeValueBase) ExtKeys() []string {
	kv := []string{
		"_start",
		"_stop",
		"_len",
		"_name",
		"_root",
		"_buffer_root",
		"_format_root",
		"_parent",
		"_actual",
		"_sym",
		"_description",
		"_path",
		"_bits",
		"_bytes",
		"_unknown",
	}

	if _, ok := dvb.dv.V.(decode.Compound); ok {
		kv = append(kv,
			"_error",
			"_format",
		)
	}

	return kv
}

func (dvb decodeValueBase) JQValueKey(name string) interface{} {
	dv := dvb.dv

	switch name {
	case "_start":
		return big.NewInt(dv.Range.Start)
	case "_stop":
		return big.NewInt(dv.Range.Stop())
	case "_len":
		return big.NewInt(dv.Range.Len)
	case "_name":
		return dv.Name
	case "_root":
		return makeDecodeValue(dv.Root())
	case "_buffer_root":
		// TODO: rename?
		return makeDecodeValue(dv.BufferRoot())
	case "_format_root":
		// TODO: rename?
		return makeDecodeValue(dv.FormatRoot())
	case "_parent":
		if dv.Parent == nil {
			return nil
		}
		return makeDecodeValue(dv.Parent)
	case "_actual":
		switch vv := dv.V.(type) {
		case decode.Scalar:
			jv, ok := gojqextra.ToGoJQValue(vv.Actual)
			if !ok {
				return fmt.Errorf("can't convert actual value jq value %#+v", vv.Actual)
			}
			return jv
		default:
			return nil
		}
	case "_sym":
		switch vv := dv.V.(type) {
		case decode.Scalar:
			jv, ok := gojqextra.ToGoJQValue(vv.Sym)
			if !ok {
				return fmt.Errorf("can't convert sym value jq value %#+v", vv.Actual)
			}
			return jv
		default:
			return nil
		}
	case "_description":
		switch vv := dv.V.(type) {
		case decode.Compound:
			if vv.Description == "" {
				return nil
			}
			return vv.Description
		case decode.Scalar:
			if vv.Description == "" {
				return nil
			}
			return vv.Description
		default:
			return nil
		}
	case "_path":
		return valuePath(dv)
	case "_error":
		switch vv := dv.V.(type) {
		case decode.Compound:
			var formatErr decode.FormatError
			if errors.As(vv.Err, &formatErr) {
				return formatErr.Value()

			}
			return vv.Err
		default:
			return nil
		}
	case "_bits":
		return BufferRange{
			bb:   dv.RootBitBuf,
			r:    dv.Range,
			unit: 1,
		}
	case "_bytes":
		return BufferRange{
			bb:   dv.RootBitBuf,
			r:    dv.Range,
			unit: 8,
		}
	case "_format":
		switch vv := dv.V.(type) {
		case decode.Compound:
			if vv.Format != nil {
				return vv.Format.Name
			}
			return nil
		case decode.Scalar:
			// TODO: hack, Scalar interface?
			switch vv.Actual.(type) {
			case map[string]interface{}, []interface{}:
				return "json"
			default:
				return nil
			}
		default:
			return nil
		}
	case "_unknown":
		switch vv := dv.V.(type) {
		case decode.Scalar:
			return vv.Unknown
		default:
			return false
		}
	}

	return expectedExtkeyError{Key: name}
}

var _ DecodeValue = decodeValue{}

type decodeValue struct {
	gojq.JQValue
	decodeValueBase
	bitsFormat bool
}

func (v decodeValue) JQValueKey(name string) interface{} {
	return valueKey(name, v.decodeValueBase.JQValueKey, v.JQValue.JQValueKey)
}
func (v decodeValue) JQValueHas(key interface{}) interface{} {
	return valueHas(key, v.decodeValueBase.JQValueKey, v.JQValue.JQValueHas)
}
func (v decodeValue) JQValueToGoJQEx(optsFn func() Options) interface{} {
	if !v.bitsFormat {
		return v.JQValueToGoJQ()
	}

	bv, err := v.decodeValueBase.ToBufferView()
	if err != nil {
		return err
	}
	bb, err := bv.toBuffer()
	if err != nil {
		return err
	}

	s, err := optsFn().BitsFormatFn(bb.Copy())
	if err != nil {
		return err
	}
	return s
}

// decode value array

var _ DecodeValue = ArrayDecodeValue{}

type ArrayDecodeValue struct {
	gojqextra.Base
	decodeValueBase
	decode.Compound
}

func NewArrayDecodeValue(dv *decode.Value, a decode.Compound) ArrayDecodeValue {
	return ArrayDecodeValue{
		decodeValueBase: decodeValueBase{dv},
		Base:            gojqextra.Base{Typ: "array"},
		Compound:        a,
	}
}

func (v ArrayDecodeValue) JQValueKey(name string) interface{} {
	return valueKey(name, v.decodeValueBase.JQValueKey, v.Base.JQValueKey)
}
func (v ArrayDecodeValue) JQValueSliceLen() interface{} { return len(*v.Compound.Children) }
func (v ArrayDecodeValue) JQValueLength() interface{}   { return len(*v.Compound.Children) }
func (v ArrayDecodeValue) JQValueIndex(index int) interface{} {
	// -1 outside after string, -2 outside before string
	if index < 0 {
		return nil
	}
	return makeDecodeValue((*v.Compound.Children)[index])
}
func (v ArrayDecodeValue) JQValueSlice(start int, end int) interface{} {
	vs := make([]interface{}, end-start)
	for i, e := range (*v.Compound.Children)[start:end] {
		vs[i] = makeDecodeValue(e)
	}
	return vs
}
func (v ArrayDecodeValue) JQValueUpdate(key interface{}, u interface{}, delpath bool) interface{} {
	return notUpdateableError{Key: fmt.Sprintf("%v", key), Typ: "array"}
}
func (v ArrayDecodeValue) JQValueEach() interface{} {
	props := make([]gojq.PathValue, len(*v.Compound.Children))
	for i, f := range *v.Compound.Children {
		props[i] = gojq.PathValue{Path: i, Value: makeDecodeValue(f)}
	}
	return props
}
func (v ArrayDecodeValue) JQValueKeys() interface{} {
	vs := make([]interface{}, len(*v.Compound.Children))
	for i := range *v.Compound.Children {
		vs[i] = i
	}
	return vs
}
func (v ArrayDecodeValue) JQValueHas(key interface{}) interface{} {
	return valueHas(
		key,
		v.decodeValueBase.JQValueKey,
		func(key interface{}) interface{} {
			intKey, ok := key.(int)
			if !ok {
				return gojqextra.HasKeyTypeError{L: "array", R: fmt.Sprintf("%v", key)}
			}
			return intKey >= 0 && intKey < len(*v.Compound.Children)
		})
}
func (v ArrayDecodeValue) JQValueToGoJQ() interface{} {
	vs := make([]interface{}, len(*v.Compound.Children))
	for i, f := range *v.Compound.Children {
		vs[i] = makeDecodeValue(f)
	}
	return vs
}

// decode value struct

var _ DecodeValue = StructDecodeValue{}

type StructDecodeValue struct {
	gojqextra.Base
	decodeValueBase
	decode.Compound
}

func NewStructDecodeValue(dv *decode.Value, s decode.Compound) StructDecodeValue {
	return StructDecodeValue{
		decodeValueBase: decodeValueBase{dv},
		Base:            gojqextra.Base{Typ: "object"},
		Compound:        s,
	}
}

func (v StructDecodeValue) JQValueLength() interface{}   { return len(*v.Compound.Children) }
func (v StructDecodeValue) JQValueSliceLen() interface{} { return len(*v.Compound.Children) }
func (v StructDecodeValue) JQValueKey(name string) interface{} {
	if strings.HasPrefix(name, "_") {
		return v.decodeValueBase.JQValueKey(name)
	}

	for _, f := range *v.Compound.Children {
		if f.Name == name {
			return makeDecodeValue(f)
		}
	}
	return nil
}
func (v StructDecodeValue) JQValueUpdate(key interface{}, u interface{}, delpath bool) interface{} {
	return notUpdateableError{Key: fmt.Sprintf("%v", key), Typ: "object"}
}
func (v StructDecodeValue) JQValueEach() interface{} {
	props := make([]gojq.PathValue, len(*v.Compound.Children))
	for i, f := range *v.Compound.Children {
		props[i] = gojq.PathValue{Path: f.Name, Value: makeDecodeValue(f)}
	}
	return props
}
func (v StructDecodeValue) JQValueKeys() interface{} {
	vs := make([]interface{}, len(*v.Compound.Children))
	for i, f := range *v.Compound.Children {
		vs[i] = f.Name
	}
	return vs
}
func (v StructDecodeValue) JQValueHas(key interface{}) interface{} {
	return valueHas(
		key,
		v.decodeValueBase.JQValueKey,
		func(key interface{}) interface{} {
			stringKey, ok := key.(string)
			if !ok {
				return gojqextra.HasKeyTypeError{L: "object", R: fmt.Sprintf("%v", key)}
			}
			for _, f := range *v.Compound.Children {
				if f.Name == stringKey {
					return true
				}
			}
			return false
		},
	)
}
func (v StructDecodeValue) JQValueToGoJQ() interface{} {
	vm := make(map[string]interface{}, len(*v.Compound.Children))
	for _, f := range *v.Compound.Children {
		vm[f.Name] = makeDecodeValue(f)
	}
	return vm
}
