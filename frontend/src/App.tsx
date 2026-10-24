import { useEffect, useState } from 'react';
import Dashboard from './pages/Dashboard';
import JobsPage from './pages/Jobs';
import NodesPage from './pages/Nodes';
import FlowsPage from './pages/Flows';
import SeriesPage from './pages/Series';
import StepsPage from './pages/Steps';
import SettingsPage from './pages/Settings';
import ProvisionPage from './pages/Provision';
import StatsPage from './pages/Stats';
import AuditPage from './pages/Audit';
import TokenGate from './components/TokenGate';
import ChangePasswordDialog from './components/ChangePasswordDialog';
import { api, hasToken, setCurrentUser, clearToken, onSessionExpired } from './api/client';

type Page = 'dashboard' | 'jobs' | 'nodes' | 'flows' | 'series' | 'steps' | 'stats' | 'audit' | 'settings' | 'provision';

// Nav is grouped by operational concern: live monitoring first, pipeline
// configuration second, and fleet-wide/system pages last. Group labels are
// rendered as .nav-label separators in the sidebar.
const NAV_GROUPS: { label: string; items: { id: Page; label: string }[] }[] = [
  {
    label: 'Monitor',
    items: [
      { id: 'dashboard', label: 'Dashboard' },
      { id: 'jobs', label: 'Jobs' },
      { id: 'nodes', label: 'Nodes' },
      { id: 'stats', label: 'Stats' },
    ],
  },
  {
    label: 'Pipeline',
    items: [
      { id: 'series', label: 'Series' },
      { id: 'flows', label: 'Flows' },
      { id: 'steps', label: 'Steps' },
      { id: 'provision', label: 'Provision' },
    ],
  },
  {
    label: 'System',
    items: [
      { id: 'audit', label: 'Audit' },
      { id: 'settings', label: 'Settings' },
    ],
  },
];

export default function App() {
  const [page, setPage] = useState<Page>('dashboard');
  const [authed, setAuthed] = useState<boolean | null>(() => (hasToken() ? null : false));
  const [who, setWho] = useState('');
  const [showChangePassword, setShowChangePassword] = useState(false);

  // If a session token is stored, validate it against the server before
  // showing anything. 401 drops straight back to the login form.
  useEffect(() => {
    onSessionExpired(() => {
      setAuthed(false);
      setWho('');
    });
    if (authed === null && hasToken()) {
      api
        .me()
        .then((me) => {
          setCurrentUser(me.username);
          setWho(me.username);
          setAuthed(true);
        })
        .catch(() => {
          clearToken();
          setAuthed(false);
        });
    }
  }, [authed]);

  async function logout() {
    try {
      await api.logout();
    } catch {
      /* session already gone; clear locally regardless */
    }
    clearToken();
    setCurrentUser('');
    setWho('');
    setAuthed(false);
  }

  if (authed === null) {
    return (
      <div className="main" style={{ maxWidth: 420, margin: '10vh auto' }}>
        <div className="card">
          <p className="muted">Checking session…</p>
        </div>
      </div>
    );
  }

  if (!authed) {
    return <TokenGate onDone={() => setAuthed(true)} />;
  }

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="sidebar-brand">
          <span className="mark" aria-hidden="true">E</span>
          <span className="name">
            encode-system
            <small>farm control</small>
          </span>
        </div>
        {NAV_GROUPS.map((g) => (
          <div className="nav-group" key={g.label}>
            <div className="nav-label">{g.label}</div>
            {g.items.map((p) => (
              <button
                key={p.id}
                className={page === p.id ? 'active' : ''}
                onClick={() => setPage(p.id)}
                aria-current={page === p.id ? 'page' : undefined}
              >
                {p.label}
              </button>
            ))}
          </div>
        ))}
        <div className="sidebar-foot">
          {who && (
            <div className="who">
              <span className="avatar" aria-hidden="true">{who.slice(0, 1)}</span>
              {who}
            </div>
          )}
          <button onClick={() => setShowChangePassword(true)}>Change password</button>
          <button onClick={logout}>Log out</button>
        </div>
      </aside>
      <main className="main">
        {page === 'dashboard' && <Dashboard />}
        {page === 'jobs' && <JobsPage />}
        {page === 'nodes' && <NodesPage />}
        {page === 'series' && <SeriesPage />}
        {page === 'flows' && <FlowsPage />}
        {page === 'steps' && <StepsPage />}
        {page === 'stats' && <StatsPage />}
        {page === 'audit' && <AuditPage />}
        {page === 'settings' && <SettingsPage />}
        {page === 'provision' && <ProvisionPage />}
      </main>
      {showChangePassword && <ChangePasswordDialog onClose={() => setShowChangePassword(false)} />}
    </div>
  );
}
