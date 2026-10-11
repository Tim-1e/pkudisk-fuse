package localfile

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsRejectsPublicCredentialACL(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "private")
	file, err := CreateTemp(dir, "credentials-*")
	if err != nil {
		t.Fatal(err)
	}
	name := file.Name()
	file.Close()
	sd, err := windows.SecurityDescriptorFromString("D:P(A;;FA;;;WD)")
	if err != nil {
		t.Fatal(err)
	}
	acl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(name, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); err != nil {
		t.Fatal(err)
	}
	if file, err := OpenPrivate(name, os.O_RDONLY); err == nil {
		file.Close()
		t.Fatal("public credential ACL was accepted")
	}
}

func TestWindowsLocalNames(t *testing.T) {
	for _, name := range []string{`..\escape`, `file:stream`, "CON", "NUL.txt", "COM1", "trailing.", "trailing ", `bad\name`, "bad*name"} {
		if ValidName(name) {
			t.Errorf("accepted unsafe Windows filename %q", name)
		}
	}
	for _, name := range []string{"报告.pdf", "ordinary.txt"} {
		if !ValidName(name) {
			t.Errorf("rejected valid filename %q", name)
		}
	}
}
