package sessionsearch

// IsCJK exports isCJK for testing.
var IsCJK = isCJK

// Tokenize exports tokenize for testing.
var Tokenize = tokenize

// Trigrams exports trigrams for testing.
var Trigrams = trigrams

// BuildPlan exports buildPlan for testing and cross-package reuse (memorytool's
// MemorySearch shares the same FTS5 MATCH-plan synthesis).
var BuildPlan = buildPlan

// LikeFragments exports likeFragments for cross-package reuse: the LIKE-fallback
// path of memorytool's MemorySearch tokenizes its query identically to
// session_search rather than duplicating the CJK splitter.
var LikeFragments = likeFragments
