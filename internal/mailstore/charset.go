package mailstore

// Registers go-message's charset decoders (ISO-8859-x, Windows-125x and the
// other golang.org/x/text encodings) for every MIME read and search path.
import _ "github.com/emersion/go-message/charset"
