import React from 'react';
import { Routes, Route, Navigate } from 'react-router-dom';
import AppLayout from './components/AppLayout';
import ProtectedRoute from './components/ProtectedRoute';
import Setup from './pages/Setup';
import Login from './pages/Login';
import Register from './pages/Register';
import Dashboard from './pages/Dashboard';
import Channels from './pages/Channels';
import RoutingRules from './pages/RoutingRules';
import Tokens from './pages/Tokens';
import TokenUsage from './pages/TokenUsage';
import Logs from './pages/Logs';
import Users from './pages/Users';
import Profile from './pages/Profile';
import Playground from './pages/Playground';
import Pricing from './pages/Pricing';

export default function App() {
  return (
    <Routes>
      {/* First screen: setup status check (routes to register or login). */}
      <Route path="/" element={<Setup />} />
      <Route path="/setup" element={<Setup />} />
      <Route path="/login" element={<Login />} />
      <Route path="/register" element={<Register />} />

      {/* Protected area behind the Semi layout. */}
      <Route
        element={
          <ProtectedRoute>
            <AppLayout />
          </ProtectedRoute>
        }
      >
        <Route path="/dashboard" element={<Dashboard />} />
        <Route path="/channels" element={<Channels />} />
        <Route path="/rules" element={<RoutingRules />} />
        <Route path="/tokens" element={<Tokens />} />
        <Route path="/tokens/:id/usage" element={<TokenUsage />} />
        <Route path="/logs" element={<Logs />} />
        <Route path="/profile" element={<Profile />} />
        {/* Admin-only routes. */}
        <Route
          path="/playground"
          element={
            <ProtectedRoute adminOnly>
              <Playground />
            </ProtectedRoute>
          }
        />
        <Route
          path="/users"
          element={
            <ProtectedRoute adminOnly>
              <Users />
            </ProtectedRoute>
          }
        />
        <Route
          path="/pricing"
          element={
            <ProtectedRoute adminOnly>
              <Pricing />
            </ProtectedRoute>
          }
        />
      </Route>

      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  );
}
