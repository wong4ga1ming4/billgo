package ticket

import "testing"

func TestPageStatesOrderAndCursor(t *testing.T) {
	all := []State{
		{ID: "b", UpdatedAt: 20},
		{ID: "a", UpdatedAt: 20},
		{ID: "c", UpdatedAt: 10},
	}
	page, next, err := pageStates(all, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page) != 2 || page[0].ID != "b" || page[1].ID != "a" {
		t.Fatalf("第一页顺序不对: %+v", page)
	}
	if next == "" {
		t.Fatal("应给出下一页游标")
	}
	rest, next2, err := pageStates(all, 2, next)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest) != 1 || rest[0].ID != "c" || next2 != "" {
		t.Fatalf("第二页不对: %+v next=%q", rest, next2)
	}
}

func TestPageStatesBadCursor(t *testing.T) {
	if _, _, err := pageStates(nil, 10, "!!!"); err == nil {
		t.Fatal("坏游标应报错")
	}
}

func TestNormalizeLimit(t *testing.T) {
	if NormalizeLimit(0) != DefaultPageLimit || NormalizeLimit(1000) != MaxPageLimit || NormalizeLimit(7) != 7 {
		t.Fatalf("limit 收束不对: %d %d %d", NormalizeLimit(0), NormalizeLimit(1000), NormalizeLimit(7))
	}
}
