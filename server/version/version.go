package version

// Version is set at build time via -ldflags "-X server/version.Version=<tag>"
// (release builds stamp the git tag name). The leading number is significant:
// the Android client (ru.yourok.torrserve) derives the server's "MatriX
// version" from the FIRST digit run in this string and gates its settings UI on
// it — e.g. it only shows the PreloadCache field (and hides the dead legacy
// PreloadBuffer switch) when that number is > 131. Keep the first number >= 132
// so the client unlocks the modern controls; "141" tracks upstream MatriX
// feature parity, ".LT-001" marks the fork.
var Version = "MatriX.141.LT-001"
