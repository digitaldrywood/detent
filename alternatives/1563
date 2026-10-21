package hubserver

import "testing"

func TestIsNetworkFilesystemType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		filesystemType int64
		want           bool
	}{
		{name: "AFS", filesystemType: afsFilesystemMagic, want: true},
		{name: "Ceph", filesystemType: cephFilesystemMagic, want: true},
		{name: "Coda", filesystemType: codaFilesystemMagic, want: true},
		{name: "NCP", filesystemType: ncpFilesystemMagic, want: true},
		{name: "NFS", filesystemType: nfsFilesystemMagic, want: true},
		{name: "SMB", filesystemType: smbFilesystemMagic, want: true},
		{name: "SMB2 and CIFS", filesystemType: smb2FilesystemMagic, want: true},
		{name: "9P", filesystemType: v9FilesystemMagic, want: true},
		{name: "ext4", filesystemType: 0xef53, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := isNetworkFilesystemType(test.filesystemType); got != test.want {
				t.Fatalf("isNetworkFilesystemType(%#x) = %t, want %t", test.filesystemType, got, test.want)
			}
		})
	}
}
