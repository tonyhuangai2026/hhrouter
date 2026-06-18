import { useState, useCallback, useEffect } from 'react';
import { Toast } from '@douyinfe/semi-ui';
import { errMessage } from '../api/helpers';

// Small reusable hook for paginated list pages (T10).
// `fetcher(params)` must return { items, total }. Pagination params are sent
// as { page, page_size } which most list endpoints accept (extra params are
// harmless if ignored by the backend).
export default function usePaginatedList(fetcher, { pageSize = 10 } = {}) {
  const [items, setItems] = useState([]);
  const [total, setTotal] = useState(0);
  const [page, setPage] = useState(1);
  const [loading, setLoading] = useState(false);

  const load = useCallback(
    async (targetPage = page) => {
      setLoading(true);
      try {
        const { items: rows, total: count } = await fetcher({
          page: targetPage,
          page_size: pageSize,
        });
        setItems(rows);
        setTotal(count);
        setPage(targetPage);
      } catch (e) {
        Toast.error(errMessage(e, 'Failed to load list'));
      } finally {
        setLoading(false);
      }
    },
    [fetcher, page, pageSize]
  );

  useEffect(() => {
    load(1);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  return { items, total, page, pageSize, loading, load, setPage };
}
