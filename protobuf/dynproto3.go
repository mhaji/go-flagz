// Copyright 2015 Michal Witkowski. All Rights Reserved.
// See LICENSE for licensing terms.

package protoflagz

import (
	"reflect"
	"sync/atomic"
	"unsafe"

	"strings"

	"github.com/golang/protobuf/jsonpb"
	"github.com/golang/protobuf/proto"
	"github.com/mwitkow/go-flagz"
	flag "github.com/spf13/pflag"
)

// DynProto3 creates a `Flag` that is backed by an arbitrary Proto3-generated datastructure which is safe to change
// dynamically at runtime either through JSONPB encoding or Proto encoding.
// The `value` must be a pointer to a struct that is JSONPB/Proto (un)marshallable.
// New values based on the default constructor of `value` type will be created on each update.
func DynProto3(flagSet *flag.FlagSet, name string, value proto.Message, usage string) *DynProto3Value {
	reflectVal := reflect.ValueOf(value)
	if reflectVal.Kind() != reflect.Ptr || reflectVal.Elem().Kind() != reflect.Struct {
		panic("DynJSON value must be a pointer to a struct")
	}
	dynValue := &DynProto3Value{ptr: unsafe.Pointer(reflectVal.Pointer()), structType: reflectVal.Type().Elem()}
	flag := flagSet.VarPF(dynValue, name, "", usage)
	flagz.MarkFlagDynamic(flag)
	return dynValue
}

// DynJSONValue is a flag-related JSON struct value wrapper.
type DynProto3Value struct {
	structType reflect.Type
	ptr        unsafe.Pointer
	validator  func(proto.Message) error
	notifier   func(oldValue proto.Message, newValue proto.Message)
}

// Get retrieves the value in its original JSON struct type in a thread-safe manner.
func (d *DynProto3Value) Get() proto.Message {
	return d.unsafeToStoredType(atomic.LoadPointer(&d.ptr)).(proto.Message)
}

// Set updates the value from a string representation in a thread-safe manner.
// This operation may return an error if the provided `input` doesn't parse, or the resulting value doesn't pass an
// optional validator.
// If a notifier is set on the value, it will be invoked in a separate go-routine.
func (d *DynProto3Value) Set(input string) error {
	someStruct := reflect.New(d.structType).Interface().(proto.Message)
	if strings.HasPrefix(strings.TrimSpace(input), "{") && strings.HasSuffix(strings.TrimSpace(input), "}") {
		if err := jsonpb.UnmarshalString(input, someStruct); err != nil {
			return err
		}
	} else {
		if err := proto.Unmarshal([]byte(input), someStruct); err != nil {
			return err
		}
	}

	if d.validator != nil {
		if err := d.validator(someStruct); err != nil {
			return err
		}
	}
	oldPtr := atomic.SwapPointer(&d.ptr, unsafe.Pointer(reflect.ValueOf(someStruct).Pointer()))
	if d.notifier != nil {
		go d.notifier(d.unsafeToStoredType(oldPtr).(proto.Message), someStruct)
	}
	return nil
}

// WithValidator adds a function that checks values before they're set.
// Any error returned by the validator will lead to the value being rejected.
// Validators are executed on the same go-routine as the call to `Set`.
func (d *DynProto3Value) WithValidator(validator func(proto.Message) error) {
	d.validator = validator
}

// WithNotifier adds a function is called every time a new value is successfully set.
// Each notifier is executed in a new go-routine.
func (d *DynProto3Value) WithNotifier(notifier func(oldValue proto.Message, newValue proto.Message)) {
	d.notifier = notifier
}

// Type is an indicator of what this flag represents.
func (d *DynProto3Value) Type() string {
	return "dyn_proto3_json"
}

// PrettyString returns a nicely structured representation of the type.
// In this case it returns a pretty-printed JSON.
func (d *DynProto3Value) PrettyString() string {
	m := &jsonpb.Marshaler{Indent: "  ", OrigName: true}
	out, err := m.MarshalToString(d.Get())
	if err != nil {
		return "ERR"
	}
	return string(out)
}

// String returns the canonical string representation of the type.
// In this case it returns the JSONPB representation of the object.
func (d *DynProto3Value) String() string {
	m := &jsonpb.Marshaler{OrigName: true}
	out, err := m.MarshalToString(d.Get())
	if err != nil {
		return "ERR"
	}
	return string(out)
}

func (d *DynProto3Value) unsafeToStoredType(p unsafe.Pointer) interface{} {
	n := reflect.NewAt(d.structType, p)
	return n.Interface()
}
