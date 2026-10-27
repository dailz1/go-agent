package skills

import (
	"context"
	"fmt"

	"github.com/dailz1/go-agent/tool"
)

func ExampleLoad() {
	set, err := Load("testdata/basic")
	if err != nil {
		fmt.Println("load error:", err)
		return
	}
	registry := tool.NewRegistry()
	if err := registry.Register(set.Tool()); err != nil {
		fmt.Println("register error:", err)
		return
	}
	result, err := set.Tool().Execute(context.Background(), []byte(`{"name":"pdf-forms"}`))
	fmt.Println("execute error:", err)
	fmt.Println(result.Content)
	// Output:
	// execute error: <nil>
	// Fill form fields with `pdfcpu fill in.pdf fields.json out.pdf`, then flatten.
}
