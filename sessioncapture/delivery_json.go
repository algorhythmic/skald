package sessioncapture

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
)

// DecodeDelivery rejects unsupported versions, omitted required fields, nulls,
// unknown core fields and duplicate core keys before semantic validation.
// Native JSON in part data and extensions remains opaque.
func DecodeDelivery(data []byte) (Delivery, error) {
	var d Delivery
	if len(data) > MaxDeliveryBytes {
		return d, errors.New("delivery_too_large")
	}
	if err := deliveryShape(data, reflect.TypeOf(d)); err != nil {
		return d, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&d); err != nil {
		return Delivery{}, fmt.Errorf("invalid_delivery_json: %w", err)
	}
	if err := dec.Decode(new(any)); err != io.EOF {
		return Delivery{}, errors.New("trailing_delivery_data")
	}
	if err := d.Validate(); err != nil {
		return Delivery{}, err
	}
	return d, nil
}

// Decode only this object's values as raw JSON so arbitrary native subtrees
// keep their existing meanings, duplicate keys and unbounded numeric lexemes.
func deliveryObject(data []byte) (map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("invalid_delivery_object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if _, exists := fields[name]; !ok || exists {
			return nil, errors.New("duplicate_delivery_field")
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		fields[name] = raw
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("trailing_delivery_data")
	}
	return fields, nil
}

// The wire struct tags define required fields; omitempty and pointer values
// retain the existing record contract's optional/nullable meanings. Raw JSON
// extensions and part data are intentionally not assigned a new schema here.
func deliveryShape(data []byte, typ reflect.Type) error {
	if typ == reflect.TypeOf(json.RawMessage{}) {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		if typ.Kind() == reflect.Pointer {
			return nil
		}
		return errors.New("unexpected_delivery_null")
	}
	if typ.Kind() == reflect.Pointer {
		return deliveryShape(data, typ.Elem())
	}
	switch typ.Kind() {
	case reflect.Struct:
		if typ == reflect.TypeOf(time.Time{}) {
			return nil
		}
		fields, err := deliveryObject(data)
		if err != nil {
			return err
		}
		for i := 0; i < typ.NumField(); i++ {
			field := typ.Field(i)
			tag := strings.Split(field.Tag.Get("json"), ",")
			raw, ok := fields[tag[0]]
			if !ok {
				if len(tag) == 1 {
					return fmt.Errorf("missing_delivery_field: %s", tag[0])
				}
				continue
			}
			if err := deliveryShape(raw, field.Type); err != nil {
				return err
			}
			delete(fields, tag[0])
		}
		if len(fields) != 0 {
			return errors.New("unknown_delivery_field")
		}
	case reflect.Map:
		fields, err := deliveryObject(data)
		if err != nil {
			return err
		}
		for _, raw := range fields {
			if err := deliveryShape(raw, typ.Elem()); err != nil {
				return err
			}
		}
	case reflect.Slice:
		if typ.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var items []json.RawMessage
		if err := json.Unmarshal(data, &items); err != nil {
			return err
		}
		for _, item := range items {
			if err := deliveryShape(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}
