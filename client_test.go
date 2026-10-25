package main

import "testing"

func TestOwnerTakeOver(t *testing.T) {
	var o owner
	if !o.claim("a") {
		t.Fatal("a cannot claim a free Laertes")
	}
	kickA, ok := o.attach("a")
	if !ok || o.claim("b") {
		t.Fatal("b claimed while a holds Laertes")
	}
	o.take("b")
	select {
	case <-kickA:
	default:
		t.Fatal("a's socket was not ended by the take-over")
	}
	o.release("a", kickA) // a's socket ending must not free b's ownership
	if o.claim("a") {
		t.Fatal("a reclaimed after b took over")
	}
	kickB, _ := o.attach("b")
	kickB2, _ := o.attach("b") // a second socket of the same page
	o.release("b", kickB)
	if o.claim("a") {
		t.Fatal("b's older socket ending freed Laertes while its newer socket is open")
	}
	o.release("b", kickB2)
	if !o.claim("a") {
		t.Fatal("Laertes not free after b's socket ended")
	}
}
