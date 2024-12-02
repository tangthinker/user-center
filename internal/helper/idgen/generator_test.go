package idgen

import "testing"

func TestCommonGenerator(t *testing.T) {

	g := NewCommonGenerator()

	id := g.Generate()

	t.Log(id)

	if len(id) == 0 {
		t.Error("id is empty")
	}

}
