package mail

import "testing"

func TestCloneSendMaterializationDeepCopiesMutableFields(t *testing.T) {
	body := "original body"
	original := &SendMaterialization{
		To:   []Recipient{{Name: "To", Address: "to@example.com"}},
		CC:   []Recipient{{Name: "CC", Address: "cc@example.com"}},
		BCC:  []Recipient{{Name: "BCC", Address: "bcc@example.com"}},
		Body: &body,
	}

	clone := cloneSendMaterialization(original)
	if clone == nil || clone.Body == nil {
		t.Fatalf("cloneSendMaterialization() = %+v, want populated clone", clone)
	}
	original.To[0].Address = "changed-to@example.com"
	original.CC[0].Address = "changed-cc@example.com"
	original.BCC[0].Address = "changed-bcc@example.com"
	*original.Body = "changed body"

	if clone.To[0].Address != "to@example.com" ||
		clone.CC[0].Address != "cc@example.com" ||
		clone.BCC[0].Address != "bcc@example.com" ||
		*clone.Body != "original body" {
		t.Fatalf("clone changed through source aliases: %+v", clone)
	}
	if cloneSendMaterialization(nil) != nil {
		t.Fatal("cloneSendMaterialization(nil) did not return nil")
	}
}
