package memsize

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"text/tabwriter"
	"unsafe"
)

// Scan traverses all objects reachable from v and counts how much memory
// is used per type. The value must be a non-nil pointer to any value.
func Scan(v interface{}, path []string) Sizes {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Ptr || rv.IsNil() {
		panic("value to scan must be non-nil pointer")
	}

	stopTheWorld("memsize scan")
	defer startTheWorld()

	ctx := newContext()
	ctx.scan(invalidAddr, rv, false, path)
	ctx.s.BitmapSize = ctx.seen.size()
	ctx.s.BitmapUtilization = ctx.seen.utilization()
	return *ctx.s
}

// Sizes is the result of a scan.
type Sizes struct {
	Total  uintptr
	ByType map[reflect.Type]*TypeSize
	// Internal stats (for debugging)
	BitmapSize        uintptr
	BitmapUtilization float32
}

type TypeSize struct {
	Total uintptr
	Count uintptr
}

func newSizes() *Sizes {
	return &Sizes{ByType: make(map[reflect.Type]*TypeSize)}
}

// Report returns a human-readable report.
func (s Sizes) Report() string {
	type typLine struct {
		name  string
		count uintptr
		total uintptr
	}
	tab := []typLine{{"ALL", 0, s.Total}}
	for _, typ := range s.ByType {
		tab[0].count += typ.Count
	}
	maxname := 0
	for typ, s := range s.ByType {
		line := typLine{typ.String(), s.Count, s.Total}
		tab = append(tab, line)
		if len(line.name) > maxname {
			maxname = len(line.name)
		}
	}
	sort.Slice(tab, func(i, j int) bool { return tab[i].total > tab[j].total })

	buf := new(bytes.Buffer)
	w := tabwriter.NewWriter(buf, 0, 0, 0, ' ', tabwriter.AlignRight)
	for _, line := range tab {
		namespace := strings.Repeat(" ", maxname-len(line.name))
		fmt.Fprintf(w, "%s%s\t  %v\t  %s\t\n", line.name, namespace, line.count, HumanSize(line.total))
	}
	w.Flush()
	return buf.String()
}

// addValue is called during scan and adds the memory of given object.
func (s *Sizes) addValue(v reflect.Value, size uintptr) {
	s.Total += size
	rs := s.ByType[v.Type()]
	if rs == nil {
		rs = new(TypeSize)
		s.ByType[v.Type()] = rs
	}
	rs.Total += size
	rs.Count++
}

type context struct {
	// We track previously scanned objects to prevent infinite loops
	// when scanning cycles and to prevent counting objects more than once.
	seen *bitmap
	tc   typCache
	s    *Sizes
}

func newContext() *context {
	return &context{seen: newBitmap(), tc: make(typCache), s: newSizes()}
}

// scan walks all objects below v, determining their size. It returns the size of the
// previously unscanned parts of the object.
func (c *context) scan(addr address, v reflect.Value, add bool, path []string) (extraSize uintptr) {
	if v.Type().Name() == "Transaction" {
		println(v.Type().String())
		println(strings.Join(path, "->"))
	}
	size := v.Type().Size()
	var marked uintptr
	if addr.valid() {
		marked = c.seen.countRange(uintptr(addr), size)
		if marked == size {
			return 0 // Skip if we have already seen the whole object.
		}
		c.seen.markRange(uintptr(addr), size)
	}
	// fmt.Printf("%v: %v ⮑ (marked %d)\n", addr, v.Type(), marked)
	if c.tc.needScan(v.Type()) {
		extraSize = c.scanContent(addr, v, path)
	}
	size -= marked
	size += extraSize
	// fmt.Printf("%v: %v %d (add %v, size %d, marked %d, extra %d)\n", addr, v.Type(), size+extraSize, add, v.Type().Size(), marked, extraSize)
	if add {
		c.s.addValue(v, size)
	}
	return size
}

// scanContent and all other scan* functions below return the amount of 'extra' memory
// (e.g. slice data) that is referenced by the object.
func (c *context) scanContent(addr address, v reflect.Value, path []string) uintptr {
	switch v.Kind() {
	case reflect.Array:
		return c.scanArray(addr, v, path)
	case reflect.Chan:
		return c.scanChan(v, path)
	case reflect.Func:
		// can't do anything here
		return 0
	case reflect.Interface:
		return c.scanInterface(v, path)
	case reflect.Map:
		return c.scanMap(v, path)
	case reflect.Ptr:
		if !v.IsNil() {
			c.scan(address(v.Pointer()), v.Elem(), true, path)
		}
		return 0
	case reflect.Slice:
		return c.scanSlice(v, path)
	case reflect.String:
		return uintptr(v.Len())
	case reflect.Struct:
		return c.scanStruct(addr, v, path)
	default:
		unhandledKind(v.Kind())
		return 0
	}
}

func (c *context) scanChan(v reflect.Value, path []string) uintptr {
	etyp := v.Type().Elem()
	extra := uintptr(0)
	if c.tc.needScan(etyp) {
		// Scan the channel buffer. This is unsafe but doesn't race because
		// the world is stopped during scan.
		hchan := unsafe.Pointer(v.Pointer())
		for i := uint(0); i < uint(v.Cap()); i++ {
			addr := chanbuf(hchan, i)
			elem := reflect.NewAt(etyp, addr).Elem()
			extra += c.scanContent(address(addr), elem, path)
		}
	}
	return uintptr(v.Cap())*etyp.Size() + extra
}

func (c *context) scanStruct(base address, v reflect.Value, path []string) uintptr {
	extra := uintptr(0)
	for i := 0; i < v.NumField(); i++ {
		f := v.Type().Field(i)
		if c.tc.needScan(f.Type) {
			path = append(path, f.Name)
			addr := base.addOffset(f.Offset)
			extra += c.scanContent(addr, v.Field(i), path)
			path = path[:len(path)-1]
		}
	}
	return extra
}

func (c *context) scanArray(addr address, v reflect.Value, path []string) uintptr {
	esize := v.Type().Elem().Size()
	extra := uintptr(0)
	for i := 0; i < v.Len(); i++ {
		extra += c.scanContent(addr, v.Index(i), path)
		addr = addr.addOffset(esize)
	}
	return extra
}

func (c *context) scanSlice(v reflect.Value, path []string) uintptr {
	slice := v.Slice(0, v.Cap())
	esize := slice.Type().Elem().Size()
	base := slice.Pointer()
	// Add size of the unscanned portion of the backing array to extra.
	blen := uintptr(slice.Len()) * esize
	marked := c.seen.countRange(base, blen)
	extra := blen - marked
	c.seen.markRange(uintptr(base), blen)
	if c.tc.needScan(slice.Type().Elem()) {
		// Elements may contain pointers, scan them individually.
		addr := address(base)
		for i := 0; i < slice.Len(); i++ {
			extra += c.scanContent(addr, slice.Index(i), path)
			addr = addr.addOffset(esize)
		}
	}
	return extra
}

func (c *context) scanMap(v reflect.Value, path []string) uintptr {
	var (
		typ   = v.Type()
		len   = uintptr(v.Len())
		extra = uintptr(0)
	)
	if c.tc.needScan(typ.Key()) || c.tc.needScan(typ.Elem()) {
		iterateMap(v, func(k, v reflect.Value) {
			extra += c.scan(invalidAddr, k, false, path)
			extra += c.scan(invalidAddr, v, false, path)
		})
	} else {
		extra = len*typ.Key().Size() + len*typ.Elem().Size()
	}
	return extra
}

func (c *context) scanInterface(v reflect.Value, path []string) uintptr {
	elem := v.Elem()
	if !elem.IsValid() {
		return 0 // nil interface
	}
	extra := c.scan(invalidAddr, elem, false, path)
	if elem.Type().Kind() == reflect.Ptr {
		extra -= uintptrBytes
	}
	return extra
}
