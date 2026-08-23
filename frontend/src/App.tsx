import { useState } from 'react';
import Dashboard from './pages/Dashboard';
import JobsPage from './pages/Jobs';
import NodesPage from './pages/Nodes';
import FlowsPage from './pages/Flows';
import TokenGate from './components/TokenGate';
import { getToken } from './api/client';

type Page = 'dashboard' | 'jobs' | 'nodes' | 'flows';

const PAGES: { id: Page; label: string }[] = [
  { id: 'dashboard', label: 'Dashboard' },
  { id: 'jobs', label: 'Jobs' },
  { id: 'nodes', label: 'Nodes' },
  { id: 'flows', label: 'Flows' },
];

export default function App() {
  const [page, setPage] = useState<Page>('dashboard');
  const [hasToken, setHasToken] = useState(() => getToken() !== '');

  if (!hasToken) {
    return <TokenGate onDone={() => setHasToken(true)} />;
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
      </aside>
      <main className="main">
        {page === 'dashboard' && <Dashboard />}
        {page === 'jobs' && <JobsPage />}
        {page === 'nodes' && <NodesPage />}
        {page === 'flows' && <FlowsPage />}
      </main>
    </div>
  );
}
