package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"mailcli/internal/mail"
)

func assertOutputSizeEvidence(t *testing.T, response envelope, requiredBytes, limitBytes int64, measured string) {
	t.Helper()
	if response.Error == nil || response.Error.Code != "output_too_large" ||
		response.Data.RequiredBytes == nil || *response.Data.RequiredBytes != requiredBytes ||
		response.Data.LimitBytes == nil || *response.Data.LimitBytes != limitBytes ||
		response.Data.Measured != measured || response.Error.RequiredBytes != nil {
		t.Fatalf("output-size evidence = data:%+v error:%+v, want required=%d limit=%d measured=%q in data",
			response.Data, response.Error, requiredBytes, limitBytes, measured)
	}
}

func TestOutputTooLargeConstructionSitesStayEnumerated(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read CLI source directory: %v", err)
	}
	want := map[string]int{
		"projection.go:writeProjectedSuccess":          1,
		"draft_workflow.go:writeBoundedDraftPreview":   1,
		"batch_commands.go:batchOutputTooLargeError":   2,
		"batch_commands.go:preflightBatchOutputBudget": 1,
	}
	got := make(map[string]int)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		if err != nil {
			t.Fatalf("parse production source %s: %v", name, err)
		}
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			ast.Inspect(function.Body, func(node ast.Node) bool {
				switch value := node.(type) {
				case *ast.CallExpr:
					if constructor, ok := value.Fun.(*ast.Ident); ok && constructor.Name == "newOutputTooLargeError" {
						got[name+":"+function.Name.Name]++
					}
				case *ast.CompositeLit:
					kind, ok := value.Type.(*ast.Ident)
					if !ok {
						return true
					}
					if kind.Name == "outputTooLargeError" && function.Name.Name != "newOutputTooLargeError" {
						t.Errorf("%s constructs outputTooLargeError outside its constructor", name)
					}
					if kind.Name == "commandError" {
						for _, element := range value.Elts {
							field, ok := element.(*ast.KeyValueExpr)
							if !ok {
								continue
							}
							key, ok := field.Key.(*ast.Ident)
							if ok && key.Name == "code" && literalString(field.Value) == "output_too_large" {
								t.Errorf("%s constructs an untyped output_too_large command error", name)
							}
						}
					}
				}
				return true
			})
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("output overflow constructor sites = %v, want %v", got, want)
	}
}

func literalString(expression ast.Expr) string {
	literal, ok := expression.(*ast.BasicLit)
	if !ok || literal.Kind != token.STRING {
		return ""
	}
	value, err := strconv.Unquote(literal.Value)
	if err != nil {
		return ""
	}
	return value
}

func TestOutputSizeEvidenceSurvivesFinalization(t *testing.T) {
	const requiredBytes, limitBytes = int64(1024), int64(512)
	limitErr := newOutputTooLargeError(requiredBytes, limitBytes, outputSizeExact, "raw")
	payload, err := marshalEnvelope(envelope{
		SchemaVersion: schemaVersion, Command: "messages.raw",
		Error: newErrorData("messages.raw", responseData{}, limitErr),
	})
	if err != nil {
		t.Fatalf("marshal output overflow: %v", err)
	}
	var stdout bytes.Buffer
	if code := FinalizeJSON(&stdout, []string{"messages", "raw", "--json"}, payload, 1, errors.New("close failed")); code != 1 {
		t.Fatalf("finalized output exit = %d, output = %s", code, stdout.String())
	}
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil {
		t.Fatalf("decode finalized output: %v; output = %s", err, stdout.String())
	}
	assertOutputSizeEvidence(t, response, requiredBytes, limitBytes, string(outputSizeExact))
	if response.Data.Finalization == nil || response.Data.Finalization.State != "failed" {
		t.Fatalf("finalization evidence was lost: %+v", response.Data.Finalization)
	}
}

func TestBatchOutputPreflightCountsStructuredOverflowEvidence(t *testing.T) {
	request := mail.BatchRequest{
		Operation:   mail.BatchOperationRead,
		Concurrency: 1,
		Items:       []mail.BatchItem{{ID: "item-one", Ref: "msg_ref"}},
	}
	maxBytes := int64(1024)
	for range 3 {
		result := mail.BatchResult{
			Operation: request.Operation, Concurrency: request.Concurrency,
			Total: len(request.Items), Completed: len(request.Items),
			Items: []mail.BatchItemResult{{ID: request.Items[0].ID, State: mail.BatchItemCompleted}},
		}
		data, err := batchResponseData(result, request, maxBytes, false, false, false, false)
		if err != nil {
			t.Fatalf("build preflight fixture: %v", err)
		}
		sample := batchOutputFailureEnvelope(result, data,
			newOutputTooLargeError(int64(^uint64(0)>>1), maxBytes, outputSizeExact, "batch"))
		payload, err := marshalEnvelope(sample)
		if err != nil {
			t.Fatalf("marshal preflight fixture: %v", err)
		}
		maxBytes = int64(len(payload) - 1)
	}

	inputPath := writeBatchInput(t, request)
	gateway := &projectionGateway{message: projectionMessage()}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), mail.NewService(gateway),
		[]string{"batch", "--input", inputPath, "--max-bytes", strconv.FormatInt(maxBytes, 10), "--json"},
		&stdout, &stderr)
	var response envelope
	if err := json.Unmarshal(stdout.Bytes(), &response); err != nil || code != 2 || response.Error == nil ||
		response.Error.Code != "invalid_argument" || gateway.getCalls != 0 || stderr.Len() != 0 {
		t.Fatalf("preflight did not reject the structured error envelope before retrieval: code=%d decode=%v calls=%d stdout=%s stderr=%s",
			code, err, gateway.getCalls, stdout.String(), stderr.String())
	}
}
