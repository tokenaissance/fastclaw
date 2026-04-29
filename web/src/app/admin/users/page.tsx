"use client";

import { useEffect, useState } from "react";
import {
  adminListUsers,
  adminCreateUser,
  adminUpdateUser,
  adminDeleteUser,
  adminResetPassword,
} from "@/lib/api";

interface UserRow {
  id: string;
  username: string;
  email: string;
  displayName?: string;
  role: string;
  status: string;
}

export default function AdminUsersPage() {
  const [users, setUsers] = useState<UserRow[]>([]);
  const [error, setError] = useState("");
  const [showCreate, setShowCreate] = useState(false);

  const [form, setForm] = useState({ username: "", email: "", password: "", displayName: "", role: "user" });

  async function refresh() {
    const res = await adminListUsers();
    if (res.users) setUsers(res.users);
    if (res.error) setError(res.error);
  }
  useEffect(() => { refresh(); }, []);

  async function handleCreate(e: React.FormEvent) {
    e.preventDefault();
    setError("");
    const res = await adminCreateUser(form);
    if (res.error) {
      setError(res.error);
      return;
    }
    setShowCreate(false);
    setForm({ username: "", email: "", password: "", displayName: "", role: "user" });
    refresh();
  }

  async function setRole(u: UserRow, role: string) {
    setError("");
    const res = await adminUpdateUser(u.id, { role });
    if (res.error) setError(res.error);
    refresh();
  }

  async function setStatus(u: UserRow, status: string) {
    setError("");
    const res = await adminUpdateUser(u.id, { status });
    if (res.error) setError(res.error);
    refresh();
  }

  async function resetPwd(u: UserRow) {
    const newPwd = prompt(`New password for ${u.username}:`);
    if (!newPwd) return;
    const res = await adminResetPassword(u.id, newPwd);
    if (res.error) setError(res.error);
  }

  async function deleteUser(u: UserRow) {
    if (!confirm(`Delete user ${u.username}? This wipes all their agents/sessions/keys.`)) return;
    const res = await adminDeleteUser(u.id);
    if (res.error) setError(res.error);
    refresh();
  }

  return (
    <div className="p-8 text-zinc-100">
      <div className="mb-6 flex items-center justify-between">
        <h1 className="text-2xl font-bold">Users</h1>
        <button
          onClick={() => setShowCreate((v) => !v)}
          className="rounded bg-violet-600 px-4 py-2 text-sm hover:bg-violet-500"
        >
          {showCreate ? "Cancel" : "+ New user"}
        </button>
      </div>

      {showCreate && (
        <form onSubmit={handleCreate} className="mb-6 space-y-3 rounded-lg border border-zinc-800 bg-zinc-900 p-4">
          <div className="grid grid-cols-2 gap-3">
            <input required value={form.username} onChange={(e) => setForm({ ...form, username: e.target.value })} placeholder="username" className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
            <input required type="email" value={form.email} onChange={(e) => setForm({ ...form, email: e.target.value })} placeholder="email" className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
          </div>
          <input required type="password" value={form.password} onChange={(e) => setForm({ ...form, password: e.target.value })} placeholder="password" className="w-full rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
          <div className="grid grid-cols-2 gap-3">
            <input value={form.displayName} onChange={(e) => setForm({ ...form, displayName: e.target.value })} placeholder="display name (optional)" className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm" />
            <select value={form.role} onChange={(e) => setForm({ ...form, role: e.target.value })} className="rounded border border-zinc-700 bg-zinc-950 px-3 py-2 text-sm">
              <option value="user">user</option>
              <option value="super_admin">super_admin</option>
            </select>
          </div>
          <button type="submit" className="rounded bg-violet-600 px-4 py-2 text-sm">Create</button>
        </form>
      )}

      {error && <p className="mb-4 text-sm text-red-400">{error}</p>}

      <table className="w-full text-sm">
        <thead className="text-left text-zinc-400">
          <tr>
            <th className="py-2">Username</th>
            <th>Email</th>
            <th>Role</th>
            <th>Status</th>
            <th></th>
          </tr>
        </thead>
        <tbody>
          {users.map((u) => (
            <tr key={u.id} className="border-t border-zinc-800">
              <td className="py-3">
                <div className="font-medium">{u.username}</div>
                {u.displayName && <div className="text-xs text-zinc-500">{u.displayName}</div>}
              </td>
              <td>{u.email}</td>
              <td>
                <select value={u.role} onChange={(e) => setRole(u, e.target.value)} className="rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-xs">
                  <option value="user">user</option>
                  <option value="super_admin">super_admin</option>
                </select>
              </td>
              <td>
                <select value={u.status} onChange={(e) => setStatus(u, e.target.value)} className="rounded border border-zinc-700 bg-zinc-950 px-2 py-1 text-xs">
                  <option value="active">active</option>
                  <option value="disabled">disabled</option>
                </select>
              </td>
              <td className="space-x-2 text-right">
                <button onClick={() => resetPwd(u)} className="text-xs text-violet-400 hover:underline">reset password</button>
                <button onClick={() => deleteUser(u)} className="text-xs text-red-400 hover:underline">delete</button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
