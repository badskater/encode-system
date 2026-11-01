import { createContext, useContext } from 'react';

// RoleContext provides the logged-in user's role ('admin' | 'viewer') to
// every page via React context, so mutation controls can be gated without
// per-page ad-hoc checks. The value is set once in App.tsx after the
// /api/auth/me call resolves; it defaults to 'admin' (the historical
// single-admin behavior) until me() returns — viewers see controls
// flash briefly then hide, which is acceptable for a LAN ops console.
export type Role = 'admin' | 'viewer';

export const RoleContext = createContext<Role>('admin');

// useRole returns the current role from context.
export function useRole(): Role {
  return useContext(RoleContext);
}

// useIsAdmin is the common gate: true when the user has admin role.
// Pages use this to hide mutation controls (create/delete/retry/etc)
// while keeping read-only tables and dashboards visible to viewers.
export function useIsAdmin(): boolean {
  return useContext(RoleContext) === 'admin';
}
