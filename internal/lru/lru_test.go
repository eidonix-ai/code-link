package lru

import "testing"

func TestCacheEvictsByCountAndCost(t *testing.T) {
	c := New[int](2, 100)
	c.Put("a", 1, 10)
	c.Put("b", 2, 10)
	c.Get("a")        // a is now most recent
	c.Put("c", 3, 10) // evicts b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should be evicted")
	}
	if v, _ := c.Get("a"); v != 1 {
		t.Fatalf("a = %d", v)
	}
	if v, _ := c.Get("c"); v != 3 {
		t.Fatalf("c = %d", v)
	}
	c.Put("d", 4, 95) // over cost: evicts until it fits
	if v, _ := c.Get("d"); c.Len() != 1 || v != 4 {
		t.Fatalf("len %d, d = %d", c.Len(), v)
	}
	c.Put("huge", 5, 101) // larger than the whole budget: not stored
	if _, ok := c.Get("huge"); ok {
		t.Fatal("huge stored")
	}
}

func TestReplacingAKeyKeepsCostRight(t *testing.T) {
	c := New[string](10, 20)
	c.Put("a", "x", 15)
	c.Put("a", "y", 15)
	if v, _ := c.Get("a"); v != "y" || c.Len() != 1 {
		t.Fatalf("got %q len %d", v, c.Len())
	}
}

func TestSHA256(t *testing.T) {
	if got := SHA256([]byte("abc")); got != "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" {
		t.Fatal(got)
	}
}
