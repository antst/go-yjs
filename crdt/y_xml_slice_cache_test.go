package crdt

import "testing"

func xmlElementNames(values ArrayAny) []string {
	names := make([]string, len(values))
	for i, value := range values {
		if element, ok := value.(*YXmlElement); ok {
			names[i] = element.NodeName
		}
	}
	return names
}

func TestYXmlSliceCacheIsolatedAndInvalidated(t *testing.T) {
	doc := newDoc("xml-slice-cache", false, defaultGCFilter, nil, false, WithClientID(1))
	fragment := doc.GetXMLFragment("x")
	fragment.Insert(0, ArrayAny{NewYXmlElement("a"), NewYXmlElement("b"), NewYXmlElement("c")})

	_ = fragment.Slice(0, 2)
	values := fragment.Slice(0, 2)
	if fragment.sliceCache.Load() == nil {
		t.Fatal("second unchanged Slice read did not populate the cache")
	}
	values[0] = nil
	if names := xmlElementNames(fragment.Slice(0, 2)); names[0] != "a" || names[1] != "b" {
		t.Fatalf("caller mutation changed cached children: %v", names)
	}

	fragment.Insert(1, ArrayAny{NewYXmlElement("inserted")})
	if fragment.sliceCache.Load() != nil || fragment.slicePrimed.Load() {
		t.Fatal("local insert did not invalidate the slice cache")
	}
	if names := xmlElementNames(fragment.Slice(0, 3)); names[1] != "inserted" {
		t.Fatalf("slice after local insert = %v", names)
	}
	fragment.Delete(0, 1)
	if names := xmlElementNames(fragment.Slice(0, 3)); len(names) != 3 || names[0] != "inserted" {
		t.Fatalf("slice after local delete = %v", names)
	}

	replica := newDoc("xml-slice-cache", false, defaultGCFilter, nil, false, WithClientID(2))
	update, err := EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, update, nil)
	remote := replica.GetXMLFragment("x")
	_, _ = remote.Slice(0, remote.GetLength()), remote.Slice(0, remote.GetLength())
	if remote.sliceCache.Load() == nil {
		t.Fatal("remote fixture did not populate the slice cache")
	}

	fragment.Insert(fragment.GetLength(), ArrayAny{NewYXmlElement("remote")})
	update, err = EncodeStateAsUpdateV2(doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = ApplyUpdateV2(replica, update, nil)
	if remote.sliceCache.Load() != nil || remote.slicePrimed.Load() {
		t.Fatal("remote insert did not invalidate the slice cache")
	}
	names := xmlElementNames(remote.Slice(0, remote.GetLength()))
	if len(names) == 0 || names[len(names)-1] != "remote" {
		t.Fatalf("slice after remote insert = %v", names)
	}
}
