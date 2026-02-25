'use client';
import { useEffect, useRef, useCallback, useState } from 'react';

const WS_URL = process.env.NEXT_PUBLIC_WS_URL || 'ws://localhost:8084';

export interface WSMessage {
  subject: string;
  data: Record<string, unknown>;
  timestamp: string;
}

export function useWebSocket(agentId?: string) {
  const ws = useRef<WebSocket | null>(null);
  const [connected, setConnected] = useState(false);
  const [messages, setMessages] = useState<WSMessage[]>([]);
  const listeners = useRef<Map<string, ((data: WSMessage) => void)[]>>(new Map());
  const reconnectTimer = useRef<NodeJS.Timeout | undefined>(undefined);
  const mounted = useRef(true);

  // Throughput tracking: timestamps of recent messages per topic
  const msgTimestamps = useRef<Map<string, number[]>>(new Map());
  const [throughput, setThroughput] = useState<Record<string, number>>({});

  // Update throughput every second
  useEffect(() => {
    const interval = setInterval(() => {
      const now = Date.now();
      const cutoff = now - 5000; // 5-second window
      const result: Record<string, number> = {};
      msgTimestamps.current.forEach((times, subject) => {
        const recent = times.filter(t => t > cutoff);
        msgTimestamps.current.set(subject, recent);
        result[subject] = parseFloat((recent.length / 5).toFixed(1));
      });
      setThroughput(result);
    }, 1000);
    return () => clearInterval(interval);
  }, []);

  const connect = useCallback(() => {
    if (!mounted.current) return;
    const url = agentId ? `${WS_URL}/ws?agent_id=${agentId}` : `${WS_URL}/ws`;
    try {
      const socket = new WebSocket(url);
      ws.current = socket;

      socket.onopen = () => {
        if (!mounted.current) return;
        setConnected(true);
        if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
      };

      socket.onmessage = (event) => {
        if (!mounted.current) return;
        try {
          const msg: WSMessage = JSON.parse(event.data);
          setMessages(prev => [msg, ...prev].slice(0, 1000));

          // Track throughput per topic
          const now = Date.now();
          if (!msgTimestamps.current.has(msg.subject)) {
            msgTimestamps.current.set(msg.subject, []);
          }
          msgTimestamps.current.get(msg.subject)!.push(now);

          // Dispatch to subject-specific listeners
          const subListeners = listeners.current.get(msg.subject) || [];
          const allListeners = listeners.current.get('*') || [];
          [...subListeners, ...allListeners].forEach(fn => fn(msg));
        } catch {
          // ignore parse errors
        }
      };

      socket.onclose = () => {
        if (!mounted.current) return;
        setConnected(false);
        ws.current = null;
        reconnectTimer.current = setTimeout(connect, 3000);
      };

      socket.onerror = () => {
        socket.close();
      };
    } catch {
      reconnectTimer.current = setTimeout(connect, 3000);
    }
  }, [agentId]);

  useEffect(() => {
    mounted.current = true;
    connect();
    return () => {
      mounted.current = false;
      if (reconnectTimer.current) clearTimeout(reconnectTimer.current);
      ws.current?.close();
    };
  }, [connect]);

  const subscribe = useCallback((subject: string, fn: (data: WSMessage) => void) => {
    if (!listeners.current.has(subject)) {
      listeners.current.set(subject, []);
    }
    listeners.current.get(subject)!.push(fn);
    return () => {
      const list = listeners.current.get(subject) || [];
      listeners.current.set(subject, list.filter(l => l !== fn));
    };
  }, []);

  return { connected, messages, subscribe, throughput };
}