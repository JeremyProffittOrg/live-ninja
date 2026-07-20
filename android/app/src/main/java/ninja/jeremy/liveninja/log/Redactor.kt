package ninja.jeremy.liveninja.log

/**
 * Strips credential material from a log message BEFORE it reaches
 * [LogSinkCore]'s ring buffer or disk (04-logging-delivery §A6 — mandatory,
 * applied in [LogSinkCore.log]). Three passes, each mopping up what the
 * previous might leave behind:
 *
 *  1. Known-sensitive header lines (`Authorization` / `X-Api-Key` / `Cookie`)
 *     — redacts the whole value on that line. Matches how
 *     `net/AuthInterceptor.kt` builds `Authorization: Bearer <jwt>` and how
 *     `net/TokenAuthenticator.kt` reads/replaces the same header.
 *  2. Bare `Bearer <token>` occurrences anywhere in the message — covers
 *     cases pass 1's line-scoped match doesn't fully consume (e.g. the
 *     header name wasn't logged, only the value).
 *  3. Bare JWT shape (`eyJ…`.`…`.`…`) anywhere in the message — catches
 *     access/refresh tokens logged without any `Bearer`/header prefix at
 *     all (e.g. `TokenAuthenticator`'s `staleToken` after
 *     `removePrefix("Bearer ")`).
 */
object Redactor {

    private val HEADER_VALUE_REGEX = Regex(
        "(?i)(Authorization|X-Api-Key|Cookie)\\s*[:=]\\s*[^\\r\\n]+",
    )
    private val BEARER_REGEX = Regex(
        "Bearer\\s+[A-Za-z0-9\\-._~+/]+=*",
        RegexOption.IGNORE_CASE,
    )
    private val JWT_REGEX = Regex(
        "eyJ[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+\\.[A-Za-z0-9_-]+",
    )

    fun redact(message: String): String {
        var out = HEADER_VALUE_REGEX.replace(message) { m -> "${m.groupValues[1]}: [REDACTED]" }
        out = BEARER_REGEX.replace(out, "Bearer [REDACTED]")
        out = JWT_REGEX.replace(out, "[REDACTED-JWT]")
        return out
    }
}
