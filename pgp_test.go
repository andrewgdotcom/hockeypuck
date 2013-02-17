/*
   Hockeypuck - OpenPGP key server
   Copyright (C) 2012  Casey Marshall

   This program is free software: you can redistribute it and/or modify
   it under the terms of the GNU Affero General Public License as published by
   the Free Software Foundation, version 3.

   This program is distributed in the hope that it will be useful,
   but WITHOUT ANY WARRANTY; without even the implied warranty of
   MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
   GNU Affero General Public License for more details.

   You should have received a copy of the GNU Affero General Public License
   along with this program.  If not, see <http://www.gnu.org/licenses/>.
*/

package hockeypuck

import (
	"bytes"
	"code.google.com/p/go.crypto/openpgp/armor"
	"github.com/bmizerany/assert"
	"testing"
)

func TestBadSelfSigUid(t *testing.T) {
	armoredKey := `-----BEGIN PGP PUBLIC KEY BLOCK-----

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
tDWgv09v+DGIYEOxrrH8yduzESTM2skXpeVhPWljEmQgSDd9hY9HZFl3BjXDPjDi
OaRa22+4PE3dwSM9brXJqO66zpEWlMPdAD1CY0MAQtcAL5AfqdcFBXIIELzP2CKu
k9/sypqTAxd6GqaoOR3mtIc/nhCAIwA1p5bKWcq7a23KwIMTR/RdCRw+V3iLrnQT
cP72aZip64Ll2yuOvd7hgOp9T4WAcmw7j9z7PzltTr/VcTS0h2mR2/uIFbxekByk
tcwXmu9jWqAelwJsye/mPcvF0g7yYF8Ix3NtGJ6jvfk4T4Xc26ERQm4c950xfo+9
C4WtwLG9AEpswKeuZpMNmT1hW/43JLfeflzi3qhKJVpInOZqDP4HrvTmrAkDdHlL
iY355Wlf1Tx6g0MSjMDAYDVQ+fsl7rWbFdOKZfwZS1FrP0HvJ4swvnkpaPco9vss
EEELi/zfdNtBGrwlefhnx6cWTQrqS3xKOf8EMUT3oska6nBTfc9iif7HkGZnmfwl
pKD/LRpRzSlDYXNleSBNYXJzaGFsbCA8Y2FzZXkubWFyc2hhbGxAZ21haWwuY29t
PsJ6BBMRCAAiBQJQmI4qAhsDBgsJCAcDAgYVCAIJCgsEFgIDAQIeAQIXgAAKCRD3
k2LaRKLR28+ZAQCWIQBTBKlJPr8fRk+MoiIywsXJxRJToykESeqqeQE59AEAgVxd
4Rf9EjrC6DJHaizxKPSE9JVEYkHDfpj5BnE0g1DRzJbMlAEQAAEBAAAAAAAAAAAA
AAAA/9j/4AAQSkZJRgABAgAAAQABAAD/2wBDAAUDBAQEAwUEBAQFBQUGBwwIBwcH
Bw8KCwkMEQ8SEhEPERATFhwXExQaFRARGCEYGhwdHx8fExciJCIeJBweHx7/2wBD
AQUFBQcGBw4ICA4eFBEUHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4eHh4e
Hh4eHh4eHh4eHh4eHh4eHh7/wAARCABkAGQDASIAAhEBAxEB/8QAHwAAAQUBAQEB
AQEAAAAAAAAAAAECAwQFBgcICQoL/8QAtRAAAgEDAwIEAwUFBAQAAAF9AQIDAAQR
BRIhMUEGE1FhByJxFDKBkaEII0KxwRVS0fAkM2JyggkKFhcYGRolJicoKSo0NTY3
ODk6Q0RFRkdISUpTVFVWV1hZWmNkZWZnaGlqc3R1dnd4eXqDhIWGh4iJipKTlJWW
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
=pBIg
-----END PGP PUBLIC KEY BLOCK-----`
	armorBlock, err := armor.Decode(bytes.NewBufferString(armoredKey))
	assert.Equal(t, nil, err)
	keyChan, errChan := ReadValidKeys(armorBlock.Body)
READING:
	for {
		select {
		case key, ok := <-keyChan:
			if !ok {
				break READING
			}
			t.Errorf("Should not get a key %v -- it's not valid", key)
		case err, ok := <-errChan:
			if !ok {
				break READING
			}
			t.Log(err)
		}
	}
}
