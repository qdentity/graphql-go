package exec

import (
	"bytes"
	"context"
	"encoding/json"
	"reflect"
	"sync"

	"github.com/qdentity/graphql-go/errors"
	"github.com/qdentity/graphql-go/internal/common"
	"github.com/qdentity/graphql-go/internal/exec/resolvable"
	"github.com/qdentity/graphql-go/internal/exec/selected"
	"github.com/qdentity/graphql-go/internal/query"
	"github.com/qdentity/graphql-go/internal/schema"
	"github.com/qdentity/graphql-go/log"
	pubquery "github.com/qdentity/graphql-go/query"
	"github.com/qdentity/graphql-go/trace"
)

type Request struct {
	selected.Request
	Limiter chan struct{}
	Tracer  trace.Tracer
	Logger  log.Logger
}

func (r *Request) handlePanic(ctx context.Context) {
	if value := recover(); value != nil {
		r.Logger.LogPanic(ctx, value)
		r.AddError(panicError(value))
	}
}

func panicError(value interface{}) *errors.QueryError {
	err := errors.Errorf("internal server error")
	err.PanicValue = value
	return err
}

func (r *Request) Execute(ctx context.Context, s *resolvable.Schema, op *query.Operation) ([]byte, []*errors.QueryError) {
	var out bytes.Buffer
	func() {
		defer r.handlePanic(ctx)
		sels := selected.ApplyOperation(&r.Request, s, op)
		r.execSelections(ctx, sels, nil, s.Resolver, &out, op.Type == query.Mutation)
	}()

	if err := ctx.Err(); err != nil {
		qErr := errors.Errorf("%s", err)
		qErr.OriginalError = err
		return nil, []*errors.QueryError{qErr}
	}

	return out.Bytes(), r.Errs
}

type fieldToExec struct {
	field    *selected.SchemaField
	sels     []selected.Selection
	resolver reflect.Value
	out      *bytes.Buffer
}

func (r *Request) execSelections(ctx context.Context, sels []selected.Selection, path *pathSegment, resolver reflect.Value, out *bytes.Buffer, serially bool) {
	async := !serially && selected.HasAsyncSel(sels)

	var fields []*fieldToExec
	collectFieldsToResolve(sels, resolver, &fields, make(map[string]*fieldToExec))

	if async {
		var wg sync.WaitGroup
		wg.Add(len(fields))
		for _, f := range fields {
			go func(f *fieldToExec) {
				defer wg.Done()
				defer r.handlePanic(ctx)
				f.out = new(bytes.Buffer)
				execFieldSelection(ctx, r, f, &pathSegment{path, f.field.Alias}, true)
			}(f)
		}
		wg.Wait()
	}

	out.WriteByte('{')
	for i, f := range fields {
		if i > 0 {
			out.WriteByte(',')
		}
		out.WriteByte('"')
		out.WriteString(f.field.Alias)
		out.WriteByte('"')
		out.WriteByte(':')
		if async {
			out.Write(f.out.Bytes())
			continue
		}
		f.out = out
		execFieldSelection(ctx, r, f, &pathSegment{path, f.field.Alias}, false)
	}
	out.WriteByte('}')
}

func collectFieldsToResolve(sels []selected.Selection, resolver reflect.Value, fields *[]*fieldToExec, fieldByAlias map[string]*fieldToExec) {
	for _, sel := range sels {
		switch sel := sel.(type) {
		case *selected.SchemaField:
			field, ok := fieldByAlias[sel.Alias]
			if !ok { // validation already checked for conflict (TODO)
				field = &fieldToExec{field: sel, resolver: resolver}
				fieldByAlias[sel.Alias] = field
				*fields = append(*fields, field)
			}
			field.sels = append(field.sels, sel.Sels...)

		case *selected.TypenameField:
			sf := &selected.SchemaField{
				Field:       resolvable.MetaFieldTypename,
				Alias:       sel.Alias,
				FixedResult: reflect.ValueOf(typeOf(sel, resolver)),
			}
			*fields = append(*fields, &fieldToExec{field: sf, resolver: resolver})

		case *selected.TypeAssertion:
			out := resolver.Method(sel.MethodIndex).Call(nil)
			if !out[1].Bool() {
				continue
			}
			collectFieldsToResolve(sel.Sels, out[0], fields, fieldByAlias)

		default:
			panic("unreachable")
		}
	}
}

func typeOf(tf *selected.TypenameField, resolver reflect.Value) string {
	if len(tf.TypeAssertions) == 0 {
		return tf.Name
	}
	for name, a := range tf.TypeAssertions {
		out := resolver.Method(a.MethodIndex).Call(nil)
		if out[1].Bool() {
			return name
		}
	}
	return ""
}

func selectionToSelectedFields(sels []selected.Selection) []pubquery.SelectedField {
	n := len(sels)
	if n == 0 {
		return nil
	}
	selectedFields := make([]pubquery.SelectedField, 0, n)
	for _, sel := range sels {
		selField, ok := sel.(*selected.SchemaField)
		if ok {
			selectedFields = append(selectedFields, pubquery.SelectedField{
				Name:     selField.Field.Name,
				Selected: selectionToSelectedFields(selField.Sels),
			})
		}
	}
	return selectedFields
}

func execFieldSelection(ctx context.Context, r *Request, f *fieldToExec, path *pathSegment, applyLimiter bool) {
	if applyLimiter {
		r.Limiter <- struct{}{}
	}

	var result reflect.Value
	var err *errors.QueryError

	traceCtx, finish := r.Tracer.TraceField(ctx, f.field.TraceLabel, f.field.TypeName, f.field.Name, !f.field.Async, f.field.Args)
	defer func() {
		finish(err)
	}()

	err = func() (err *errors.QueryError) {
		defer func() {
			if panicValue := recover(); panicValue != nil {
				r.Logger.LogPanic(ctx, panicValue)
				err = panicError(panicValue)
				err.Path = path.toSlice()
			}
		}()

		if f.field.FixedResult.IsValid() {
			result = f.field.FixedResult
			return nil
		}

		if err := traceCtx.Err(); err != nil {
			return errors.Errorf("%s", err) // don't execute any more resolvers if context got cancelled
		}

		var in []reflect.Value
		if f.field.HasContext {
			in = append(in, reflect.ValueOf(traceCtx))
		}
		if f.field.ArgsPacker != nil {
			in = append(in, f.field.PackedArgs)
		}
		if f.field.HasSelected {
			in = append(in, reflect.ValueOf(selectionToSelectedFields(f.sels)))
		}
		callOut := f.resolver.Method(f.field.MethodIndex).Call(in)
		result = callOut[0]
		if f.field.HasError && !callOut[1].IsNil() {
			resolverErr := callOut[1].Interface().(error)
			err := errors.Errorf("%s", resolverErr)
			err.Path = path.toSlice()
			err.OriginalError = resolverErr
			return err
		}
		return nil
	}()

	if applyLimiter {
		<-r.Limiter
	}

	if err != nil {
		r.AddError(err)
		f.out.WriteString("null") // TODO handle non-nil
		return
	}

	r.execSelectionSet(traceCtx, f.sels, f.field.Type, path, result, f.out)
}

func (r *Request) execSelectionSet(ctx context.Context, sels []selected.Selection, typ common.Type, path *pathSegment, resolver reflect.Value, out *bytes.Buffer) {
	t, nonNull := unwrapNonNull(typ)
	switch t := t.(type) {
	case *schema.Object, *schema.Interface, *schema.Union:
		if resolver.Kind() == reflect.Ptr && resolver.IsNil() {
			if nonNull {
				panic(errors.Errorf("got nil for non-null %q", t))
			}
			out.WriteString("null")
			return
		}

		r.execSelections(ctx, sels, path, resolver, out, false)
		return
	}

	if !nonNull {
		if resolver.IsNil() {
			out.WriteString("null")
			return
		}
		resolver = resolver.Elem()
	}

	switch t := t.(type) {
	case *common.List:
		l := resolver.Len()

		if selected.HasAsyncSel(sels) {
			var wg sync.WaitGroup
			wg.Add(l)
			entryouts := make([]bytes.Buffer, l)
			for i := 0; i < l; i++ {
				go func(i int) {
					defer wg.Done()
					defer r.handlePanic(ctx)
					r.execSelectionSet(ctx, sels, t.OfType, &pathSegment{path, i}, resolver.Index(i), &entryouts[i])
				}(i)
			}
			wg.Wait()

			out.WriteByte('[')
			for i, entryout := range entryouts {
				if i > 0 {
					out.WriteByte(',')
				}
				out.Write(entryout.Bytes())
			}
			out.WriteByte(']')
			return
		}

		out.WriteByte('[')
		for i := 0; i < l; i++ {
			if i > 0 {
				out.WriteByte(',')
			}
			r.execSelectionSet(ctx, sels, t.OfType, &pathSegment{path, i}, resolver.Index(i), out)
		}
		out.WriteByte(']')

	case *schema.Scalar:
		v := resolver.Interface()
		data, err := json.Marshal(v)
		if err != nil {
			panic(errors.Errorf("could not marshal %v", v))
		}
		out.Write(data)

	case *schema.Enum:
		out.WriteByte('"')
		out.WriteString(resolver.String())
		out.WriteByte('"')

	default:
		panic("unreachable")
	}
}

func unwrapNonNull(t common.Type) (common.Type, bool) {
	if nn, ok := t.(*common.NonNull); ok {
		return nn.OfType, true
	}
	return t, false
}

type pathSegment struct {
	parent *pathSegment
	value  interface{}
}

func (p *pathSegment) toSlice() []interface{} {
	if p == nil {
		return nil
	}
	return append(p.parent.toSlice(), p.value)
}
