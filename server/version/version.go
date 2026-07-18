package version

// Version is set at build time via -ldflags "-X server/version.Version=<tag>"
// (release builds stamp the git tag name). The leading number is significant:
// the Android client (ru.yourok.torrserve) derives the server's "MatriX
// version" from the FIRST digit run in this string and gates its settings UI on
// it — e.g. it only shows the PreloadCache field (and hides the dead legacy
// PreloadBuffer switch) when that number is > 131. Keep the first number >= 132
// so the client unlocks the modern controls; "142" tracks upstream MatriX
// feature parity (and is that first digit run), ".LT-1.0.0" marks the fork and
// carries its own semantic version. Release tags must keep this shape
// (MatriX.142.LT-X.Y.Z) so the gate stays satisfied.
var Version = "MatriX.142.LT-1.1.1"
