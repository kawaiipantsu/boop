// Access-token handling for a server started with web auth enabled (§23).
//
// The token is never bundled and never logged. It is read from the page URL
// once (so a shared link works), immediately stripped from the address bar,
// and kept in sessionStorage for the life of the tab only.

const STORAGE_KEY = 'boop.access_token';
/** Matches web/auth.go tokenQueryParam. */
export const TOKEN_QUERY_PARAM = 'access_token';

let cached: string | null = null;

function storage(): Storage | null {
  try {
    return window.sessionStorage;
  } catch {
    return null;
  }
}

/** Reads the token from the URL on first call, then from session storage. */
export function accessToken(): string {
  if (cached !== null) return cached;
  let token = '';
  try {
    const url = new URL(window.location.href);
    token = url.searchParams.get(TOKEN_QUERY_PARAM) ?? url.searchParams.get('token') ?? '';
    if (token !== '') {
      url.searchParams.delete(TOKEN_QUERY_PARAM);
      url.searchParams.delete('token');
      window.history.replaceState(null, '', url.toString());
      storage()?.setItem(STORAGE_KEY, token);
    } else {
      token = storage()?.getItem(STORAGE_KEY) ?? '';
    }
  } catch {
    token = '';
  }
  cached = token;
  return token;
}

/** Test seam. */
export function resetTokenCache(): void {
  cached = null;
}
