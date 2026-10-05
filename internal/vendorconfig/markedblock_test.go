package vendorconfig

import "testing"

func TestMarkedBlockUpsertReplaceRemove(t *testing.T) {
	const begin, end = "# >>> x >>>", "# <<< x <<<"
	block := begin + "\na = 1\n" + end
	out, changed, err := UpsertMarkedBlock("model = \"m\"\n", begin, end, block)
	if err != nil || !changed || out != "model = \"m\"\n\n"+block+"\n" {
		t.Fatalf("append: %q %v %v", out, changed, err)
	}
	if again, changed, _ := UpsertMarkedBlock(out, begin, end, block); changed || again != out {
		t.Fatal("an equal block must be a no-op")
	}
	replaced, changed, _ := UpsertMarkedBlock(out, begin, end, begin+"\na = 2\n"+end)
	if !changed || MarkedBlock(replaced, begin, end) != begin+"\na = 2\n"+end {
		t.Fatalf("replace: %q", replaced)
	}
	removed, ok, err := RemoveMarkedBlock(replaced, begin, end)
	if err != nil || !ok || removed != "model = \"m\"\n" {
		t.Fatalf("remove: %q %v %v", removed, ok, err)
	}
	if _, _, err := UpsertMarkedBlock(begin+"\nno end", begin, end, block); err == nil {
		t.Fatal("a begin marker without an end must be refused")
	}
	if _, _, err := UpsertMarkedBlock("", begin, end, "not a block"); err == nil {
		t.Fatal("a block without its markers must be refused")
	}
}
