import { createBrowserRouter, Navigate } from 'react-router-dom'
import { AppShell } from '../components/shell/AppShell'
import SettingsLayout from '../components/shell/SettingsLayout'
import OverviewPage from '../features/overview/OverviewPage'
import HostsPage from '../features/hosts/HostsPage'
import LoadBalancerPage from '../features/loadbalancer/LoadBalancerPage'
import CertificatesPage from '../features/certificates/CertificatesPage'
import AccessListsPage from '../features/access/AccessListsPage'
import StreamsPage from '../features/streams/StreamsPage'
import LogsPage from '../features/logs/LogsPage'
import HistoryPage from '../features/history/HistoryPage'
import LoginPage from '../features/auth/LoginPage'
import SetupWizard from '../features/auth/SetupWizard'
import DocsPage from '../features/docs/DocsPage'
import DNSPage from '../features/dns/DNSPage'
import TopologyPage from '../features/topology/TopologyPage'
import HostFlowPage from '../features/topology/HostFlowPage'

export const router = createBrowserRouter([
  { path: '/login', element: <LoginPage /> },
  { path: '/setup', element: <SetupWizard /> },
  {
    path: '/',
    element: <AppShell />,
    children: [
      { index: true, element: <OverviewPage /> },
      { path: 'topology', element: <TopologyPage /> },
      { path: 'topology/hosts/:id', element: <HostFlowPage /> },
      { path: 'hosts', element: <HostsPage /> },
      { path: 'hosts/:tab', element: <HostsPage /> },
      { path: 'load-balancer', element: <Navigate to="/load-balancer/backends" replace /> },
      { path: 'load-balancer/:tab', element: <LoadBalancerPage /> },
      { path: 'certificates', element: <CertificatesPage /> },
      { path: 'access', element: <AccessListsPage /> },
      { path: 'streams', element: <StreamsPage /> },
      { path: 'dns', element: <DNSPage /> },
      { path: 'logs', element: <Navigate to="/logs/access" replace /> },
      { path: 'logs/:tab', element: <LogsPage /> },
      { path: 'history', element: <HistoryPage /> },
      { path: 'docs', element: <Navigate to="/docs/introduction" replace /> },
      { path: 'docs/:section', element: <DocsPage /> },
      { path: 'settings', element: <Navigate to="/settings/general" replace /> },
      { path: 'settings/:section', element: <SettingsLayout /> },
      { path: '*', element: <Navigate to="/" replace /> },
    ],
  },
])
