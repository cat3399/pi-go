package application

import "reflect"

// Account for retained typed payloads, including private immutable message
// fields which JSON reflection would serialize as {}. This is a conservative
// payload estimate, not an RSS measurement; transport encoding is independent.
func estimatedEventBytes(event Event) int {
	seen := make(map[uintptr]bool)
	var size func(reflect.Value, int) int
	size = func(value reflect.Value, depth int) int {
		if !value.IsValid() {
			return 0
		}
		if depth > 32 {
			return defaultEventHistoryBytes + 1
		}
		base := int(value.Type().Size())
		switch value.Kind() {
		case reflect.String:
			return base + value.Len()
		case reflect.Interface:
			if value.IsNil() {
				return base
			}
			return base + size(value.Elem(), depth+1)
		case reflect.Pointer:
			if value.IsNil() || seen[value.Pointer()] {
				return base
			}
			seen[value.Pointer()] = true
			return base + size(value.Elem(), depth+1)
		case reflect.Slice:
			if value.Type().Elem().Kind() == reflect.Uint8 {
				return base + value.Cap()
			}
			if value.IsNil() || seen[value.Pointer()] {
				return base
			}
			seen[value.Pointer()] = true
			base += (value.Cap() - value.Len()) * int(value.Type().Elem().Size())
			for i := 0; i < value.Len(); i++ {
				base += size(value.Index(i), depth+1)
			}
		case reflect.Array:
			for i := 0; i < value.Len(); i++ {
				base += size(value.Index(i), depth+1)
			}
		case reflect.Struct:
			for i := 0; i < value.NumField(); i++ {
				base += size(value.Field(i), depth+1)
			}
		case reflect.Map:
			pointer := uintptr(value.UnsafePointer())
			if value.IsNil() || seen[pointer] {
				return base
			}
			seen[pointer] = true
			iterator := value.MapRange()
			for iterator.Next() {
				base += 16 + size(iterator.Key(), depth+1) + size(iterator.Value(), depth+1)
			}
		}
		return base
	}
	return size(reflect.ValueOf(event), 0)
}
