package onboardingpage

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/onboarding"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

type Column struct {
	Name string
	Type string
}

type field struct {
	typeOf   reflect.Type
	column   int
	children []*field
	indexes  []int
}

type Layout struct {
	Columns []Column
	root    *field
}

func NewLayout() *Layout {
	l := &Layout{}
	l.root = l.add(reflect.TypeFor[onboarding.Project](), "page")
	return l
}

func (l *Layout) add(t reflect.Type, name string) *field {
	if t == reflect.TypeFor[*policy.Configuration]() || t == reflect.TypeFor[*policy.ExecutionConfiguration]() ||
		t == reflect.TypeFor[[]policy.WorkflowApply]() ||
		t == reflect.TypeFor[[]runnerauth.RunnerLease]() ||
		t == reflect.TypeFor[[]providercapacity.View]() {
		return nil
	}
	f := &field{typeOf: t, column: -1}
	kind := ""
	switch {
	case t == reflect.TypeFor[time.Time]():
		kind = "timestamptz"
	case t.Kind() == reflect.Struct:
		for i := range t.NumField() {
			member := t.Field(i)
			tag := strings.Split(member.Tag.Get("json"), ",")[0]
			if !member.IsExported() || tag == "-" {
				continue
			}
			if tag == "" {
				tag = strings.ToLower(member.Name)
			}
			child := l.add(member.Type, name+"_"+tag)
			if child != nil {
				f.children = append(f.children, child)
				f.indexes = append(f.indexes, i)
			}
		}
	case t.Kind() == reflect.Pointer:
		kind = "boolean"
		f.children = []*field{l.add(t.Elem(), name+"_value")}
	case t.Kind() == reflect.Slice:
		kind = "bigint"
		f.children = []*field{l.add(t.Elem(), name+"_item")}
	case t.Kind() == reflect.Map:
		kind = "bigint"
		f.children = []*field{l.add(t.Key(), name+"_key"), l.add(t.Elem(), name+"_value")}
	case t.Kind() == reflect.String:
		kind = "text"
	case t.Kind() == reflect.Bool:
		kind = "boolean"
	case t.Kind() >= reflect.Int && t.Kind() <= reflect.Int64:
		kind = "bigint"
	case t.Kind() == reflect.Float64:
		kind = "double precision"
	default:
		panic(fmt.Sprintf("unsupported onboarding display field %s: %s", name, t))
	}
	if kind != "" {
		if len(name) > 63 {
			name = fmt.Sprintf("%s_%x", name[:46], sha256.Sum256([]byte(name)))[:63]
		}
		f.column = len(l.Columns)
		l.Columns = append(l.Columns, Column{Name: name, Type: kind + "[]"})
	}
	return f
}

func (l *Layout) Values(value onboarding.Project) []any {
	values := make([]reflect.Value, len(l.Columns))
	for i, c := range l.Columns {
		var t reflect.Type
		switch c.Type {
		case "text[]":
			t = reflect.TypeFor[[]string]()
		case "bigint[]":
			t = reflect.TypeFor[[]int64]()
		case "boolean[]":
			t = reflect.TypeFor[[]bool]()
		case "timestamptz[]":
			t = reflect.TypeFor[[]time.Time]()
		case "double precision[]":
			t = reflect.TypeFor[[]float64]()
		}
		values[i] = reflect.MakeSlice(t, 0, 0)
	}
	l.root.encode(reflect.ValueOf(value), values)
	result := make([]any, len(values))
	for i, v := range values {
		result[i] = v.Interface()
	}
	return result
}

func (f *field) encode(v reflect.Value, columns []reflect.Value) {
	appendValue := func(value any) { columns[f.column] = reflect.Append(columns[f.column], reflect.ValueOf(value)) }
	switch {
	case f.typeOf == reflect.TypeFor[time.Time]():
		appendValue(v.Interface())
	case v.Kind() == reflect.Struct:
		for i, child := range f.children {
			child.encode(v.Field(f.indexes[i]), columns)
		}
	case v.Kind() == reflect.Pointer:
		appendValue(!v.IsNil())
		if !v.IsNil() {
			f.children[0].encode(v.Elem(), columns)
		}
	case v.Kind() == reflect.Slice || v.Kind() == reflect.Map:
		size := v.Len()
		if v.IsNil() {
			size = -1
		}
		appendValue(int64(size))
		if v.Kind() == reflect.Slice {
			for i := range v.Len() {
				f.children[0].encode(v.Index(i), columns)
			}
			return
		}
		keys := v.MapKeys()
		slices.SortFunc(keys, func(a, b reflect.Value) int { return strings.Compare(a.String(), b.String()) })
		for _, key := range keys {
			f.children[0].encode(key, columns)
			f.children[1].encode(v.MapIndex(key), columns)
		}
	case v.Kind() == reflect.String:
		appendValue(v.String())
	case v.Kind() == reflect.Bool:
		appendValue(v.Bool())
	case v.Kind() == reflect.Float64:
		appendValue(v.Float())
	default:
		appendValue(v.Int())
	}
}

func (l *Layout) Destinations() []any {
	values := l.Values(onboarding.Project{})
	for i, v := range values {
		values[i] = reflect.New(reflect.TypeOf(v)).Interface()
	}
	return values
}

func (l *Layout) Project(values []any) (onboarding.Project, error) {
	var result onboarding.Project
	columns := make([]reflect.Value, len(values))
	for i, v := range values {
		columns[i] = reflect.ValueOf(v).Elem()
	}
	positions := make([]int, len(values))
	err := l.root.decode(reflect.ValueOf(&result).Elem(), columns, positions)
	if err == nil {
		for i, v := range columns {
			if positions[i] != v.Len() {
				return result, fmt.Errorf("invalid onboarding column %s", l.Columns[i].Name)
			}
		}
	}
	return result, err
}

func (f *field) decode(v reflect.Value, columns []reflect.Value, positions []int) error {
	var value reflect.Value
	if f.column >= 0 {
		if positions[f.column] >= columns[f.column].Len() {
			return errors.New("incomplete onboarding display field")
		}
		value = columns[f.column].Index(positions[f.column])
		positions[f.column]++
	}
	switch {
	case f.typeOf == reflect.TypeFor[time.Time]():
		v.Set(value)
	case v.Kind() == reflect.Struct:
		for i, child := range f.children {
			if err := child.decode(v.Field(f.indexes[i]), columns, positions); err != nil {
				return err
			}
		}
	case v.Kind() == reflect.Pointer:
		if value.Bool() {
			v.Set(reflect.New(v.Type().Elem()))
			return f.children[0].decode(v.Elem(), columns, positions)
		}
	case v.Kind() == reflect.Slice || v.Kind() == reflect.Map:
		size := value.Int()
		if size == -1 {
			return nil
		}
		if size < 0 || size > 1000000 {
			return errors.New("invalid onboarding collection size")
		}
		if v.Kind() == reflect.Slice {
			v.Set(reflect.MakeSlice(v.Type(), int(size), int(size)))
			for i := range int(size) {
				if err := f.children[0].decode(v.Index(i), columns, positions); err != nil {
					return err
				}
			}
		} else {
			v.Set(reflect.MakeMapWithSize(v.Type(), int(size)))
			for range int(size) {
				key, item := reflect.New(v.Type().Key()).Elem(), reflect.New(v.Type().Elem()).Elem()
				if err := f.children[0].decode(key, columns, positions); err != nil {
					return err
				}
				if err := f.children[1].decode(item, columns, positions); err != nil {
					return err
				}
				v.SetMapIndex(key, item)
			}
		}
	default:
		v.Set(value.Convert(v.Type()))
	}
	return nil
}
