import * as React from 'react';
import useClient from './useClient';
import { filterRequestByName, filterScalesByState } from './client';
import { ScaleRequest, ScaleRequestState } from './generated/controller_pb';

export default function useAppScale(appName: string) {
	const client = useClient();
	const [loading, setLoading] = React.useState(true);
	const [scale, setScale] = React.useState<ScaleRequest | null>(null);
	const [error, setError] = React.useState<Error | null>(null);
	React.useEffect(
		() => {
			const cancel = client.streamScales(
				(scales: ScaleRequest[], error: Error | null) => {
					if (error) {
						setError(error);
						return;
					}
					if (scales.length === 0) {
						// TODO(jvatic): set an empty one
						return;
					}
					setScale(scales[0]);
					setLoading(false);
					setError(null);
				},
				filterRequestByName(appName),
				filterScalesByState(ScaleRequestState.SCALE_COMPLETE)
			);
			return cancel;
		},
		[appName, client]
	);
	return {
		loading,
		scale,
		error
	};
}
