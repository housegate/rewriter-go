package main

import (
	"strings"
	"testing"

	rewriter "github.com/housegate/rewriter-go"
	"github.com/housegate/rewriter-proto/gen/pb"
	"google.golang.org/protobuf/encoding/protojson"
)

func TestResponseFromResultCarriesStorageIntegrityAcknowledgement(t *testing.T) {
	got := responseFromResult(rewriter.RewriteResult{
		SQL:                             "SELECT 1",
		Code:                            pb.RewriteCode_Success,
		StorageIntegrityContractVersion: pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1,
	})
	if got.GetStorageIntegrityContractVersion() != pb.StorageIntegrityContractVersion_STORAGE_INTEGRITY_CONTRACT_V1 {
		t.Fatalf("storage_integrity_contract_version=%v, want V1", got.GetStorageIntegrityContractVersion())
	}
	out, err := protojson.MarshalOptions{UseProtoNames: true}.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"storage_integrity_contract_version":"STORAGE_INTEGRITY_CONTRACT_V1"`) {
		t.Fatalf("CLI protojson output dropped v1 acknowledgement: %s", out)
	}
}
