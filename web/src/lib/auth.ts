const TOKEN_KEY = "pyke_token";
// v0.5.0 更名（pojun→pyke）过渡：旧 key 一个版本周期后移除。
const LEGACY_TOKEN_KEY = "pojun_token";
const COOKIE_MAX_AGE = 7 * 24 * 60 * 60; // 7 天（秒）

export interface CurrentUser {
  id: string;
  name: string;
  username: string;
  email: string;
  avatar: string;
  role: string;
}

export const auth = {
  getToken(): string | null {
    if (typeof window === "undefined") return null;
    // Mock demo：无真实登录，返回一个假 token 让路由守卫放行、直接进主界面。
    // 更名过渡：旧 key 里还有 token 就先迁移到新 key（旧标签页/存储无缝续期）。
    const legacy = localStorage.getItem(LEGACY_TOKEN_KEY);
    const current = localStorage.getItem(TOKEN_KEY);
    if (!current && legacy) {
      this.setToken(legacy);
      return legacy;
    }
    return current ?? (process.env.NEXT_PUBLIC_MOCK === "1" ? "mock-demo" : null);
  },

  setToken(token: string): void {
    localStorage.setItem(TOKEN_KEY, token);
    // 同步写 cookie，供 Next.js middleware 服务端读取
    document.cookie = `${TOKEN_KEY}=${encodeURIComponent(token)}; path=/; max-age=${COOKIE_MAX_AGE}; SameSite=Lax`;
  },

  clearToken(): void {
    localStorage.removeItem(TOKEN_KEY);
    localStorage.removeItem(LEGACY_TOKEN_KEY);
    document.cookie = `${TOKEN_KEY}=; path=/; max-age=0`;
    document.cookie = `${LEGACY_TOKEN_KEY}=; path=/; max-age=0`;
  },

  // 从 JWT payload 的 sub 字段解析当前用户，仅用于展示，不做签名验证。
  getCurrentUser(): CurrentUser | null {
    const token = this.getToken();
    if (!token) return null;
    try {
      const parts = token.split(".");
      if (parts.length !== 3) return null;
      // base64url → base64
      const payload = JSON.parse(atob(parts[1].replace(/-/g, "+").replace(/_/g, "/")));
      const username: string = payload.sub ?? "PYKE";
      return { id: "1", name: username, username, email: "", avatar: "", role: "operator" };
    } catch {
      return null;
    }
  },
};
