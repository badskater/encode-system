import { useEffect, useState } from 'react';
import Dashboard from './pages/Dashboard';
import JobsPage from './pages/Jobs';
import NodesPage from './pages/Nodes';
import FlowsPage from './pages/Flows';
import SeriesPage from './pages/Series';
import StepsPage from './pages/Steps';
import TokenGate from './components/TokenGate';
import { api, hasToken, setCurrentUser, clearToken, onSessionExpired } from './api/client';

type Page = 'dashboard' | 'jobs' | 'nodes' | 'flows' | 'series' | 'steps';

const PAGES: { id: Page; label: string }[] = [
  { id: 'dashboard', label: 'Dashboard' },
  { id: 'jobs', label: 'Jobs' },
  { id: 'nodes', label: 'Nodes' },
  { id: 'series', label: 'Series' },
  { id: 'flows', label: 'Flows' },
  { id: 'steps', label: 'Steps' },
];

export default function App() {
  const [page, setPage] = useState<Page>('dashboard');
  const [authed, setAuthed] = useState<boolean | null>(() => (hasToken() ? null : false));
  const [who, setWho] = useState('');

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
        <h1>encode-system</h1>
        {PAGES.map((p) => (
          <button
            key={p.id}
            className={page === p.id ? 'active' : ''}
            onClick={() => setPage(p.id)}
          >
            {p.label}
          </button>
        ))}
        <div style={{ marginTop: 'auto', paddingTop: 16 }}>
          {who && <div className="muted" style={{ marginBottom: 6 }}>{who}</div>}
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
      </main>
    </div>
  );
}
