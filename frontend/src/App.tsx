import { useState } from 'react';
import Dashboard from './pages/Dashboard';
import JobsPage from './pages/Jobs';
import NodesPage from './pages/Nodes';
import FlowsPage from './pages/Flows';
import SeriesPage from './pages/Series';
import StepsPage from './pages/Steps';
import TokenGate from './components/TokenGate';
import { getToken } from './api/client';

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
        {page === 'series' && <SeriesPage />}
        {page === 'flows' && <FlowsPage />}
        {page === 'steps' && <StepsPage />}
      </main>
    </div>
  );
}
