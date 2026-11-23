const { createApp, ref, computed, onMounted, onUnmounted } = Vue;

createApp({
  setup() {
    const health = ref({ ok:false, version:'-', upstreams:[], started_at:'' });
    const stats = ref({ total:0, succeeded:0, failed:0, retried:0, upstream5xx:0 });
    const cfg = ref({});
    const requests = ref([]);
    const filter = ref('');
    const onlyIssues = ref(false);
    const sseState = ref('dead');
    let es = null, tick = null, uptimeTick = null, seq = 0;

    const sseLabel = computed(() =>
      sseState.value === 'live' ? 'SSE 实时' :
      sseState.value === 'connecting' ? 'SSE 连接中' : 'SSE 断开');

    const successRate = computed(() => {
      const t = stats.value.total || 0;
      if (!t) return '—';
      return ((stats.value.succeeded / t) * 100).toFixed(1) + '%';
    });

    const uptimeText = computed(() => {
      if (!health.value.started_at) return '—';
      const s = Math.max(0, Math.floor((Date.now() - new Date(health.value.started_at)) / 1000));
      const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600),
            m = Math.floor((s % 3600) / 60), sec = s % 60;
      if (d) return `${d}天${h}小时`;
      if (h) return `${h}小时${m}分`;
      if (m) return `${m}分${sec}秒`;
      return `${sec}秒`;
    });

    const filtered = computed(() => {
      const f = filter.value.trim().toLowerCase();
      return requests.value.filter(r => {
        if (onlyIssues.value && !(r.attempt > 1 || (r.status && r.status >= 400) || r.error)) return false;
        if (!f) return true;
        return String(r.id).toLowerCase().includes(f)
            || String(r.path).toLowerCase().includes(f)
            || String(r.status).includes(f)
            || String(r.note || '').toLowerCase().includes(f);
      });
    });

    const fmtBytes = n => {
      if (!n) return '—';
      if (n < 1024) return n + ' B';
      if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
      return (n / 1048576).toFixed(2) + ' MB';
    };
    const fmtDur = ms => {
      if (ms == null) return '—';
      if (ms < 1000) return ms + ' ms';
      return (ms / 1000).toFixed(2) + ' s';
    };
    const statusClass = s => !s ? 'sn' : (s < 400 ? 's2' : s < 500 ? 's4' : 's5');
    const rowClass = r => (r.status && r.status >= 400) || r.error ? 'fail'
                        : (r.attempt > 1 ? 'retry' : '');

    async function loadAll() {
      try {
        const [h, s, c] = await Promise.all([
          fetch('/api/health').then(r => r.json()),
          fetch('/api/stats').then(r => r.json()),
          fetch('/api/config').then(r => r.json()),
        ]);
        health.value = h;
        stats.value = s;
        cfg.value = c;
      } catch (e) { /* 忽略瞬时失败 */ }
    }

    function addRecord(rec) {
      rec.seq = ++seq;
      requests.value.unshift(rec);
      if (requests.value.length > 300) requests.value.length = 300;
    }

    function connectSSE() {
      if (es) es.close();
      sseState.value = 'connecting';
      es = new EventSource('/api/requests/stream');
      es.addEventListener('open', () => { sseState.value = 'live'; });
      es.addEventListener('record', ev => {
        try { addRecord(JSON.parse(ev.data)); } catch (e) {}
      });
      es.addEventListener('stats', ev => {
        try { stats.value = JSON.parse(ev.data); } catch (e) {}
      });
      es.addEventListener('error', () => {
        sseState.value = 'dead';
        es.close();
        setTimeout(connectSSE, 3000);
      });
    }

    function clearLocal() { requests.value = []; }

    onMounted(async () => {
      await loadAll();
      try {
        const hist = await fetch('/api/requests?limit=200').then(r => r.json());
        (hist.records || []).forEach(addRecord);
      } catch (e) {}
      connectSSE();
      tick = setInterval(loadAll, 5000);
      uptimeTick = setInterval(() => { health.value = { ...health.value }; }, 1000);
    });
    onUnmounted(() => {
      if (es) es.close();
      clearInterval(tick); clearInterval(uptimeTick);
    });

    return {
      health, stats, cfg, requests, filter, onlyIssues, sseState, sseLabel,
      successRate, uptimeText, filtered, fmtBytes, fmtDur, statusClass, rowClass, clearLocal,
    };
  },
}).mount('#app');
